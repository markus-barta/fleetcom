// Package openclaw is FleetCom's client for the OpenClaw gateway WS RPC
// protocol. It runs one WebSocket connection per paired gateway and
// implements the auto-approval loop that turns bridge registrations
// (POST /api/bridges/register) into approved device pairings on the
// gateway without any human in the loop.
//
// The wire protocol (handshake payload v3, Ed25519 signature format,
// base64url encoding rules, RPC envelope shape) was reverse-engineered
// from OpenClaw's bundled dist/*.js — see docs/AGENT-BRIDGE-PAIRING.md
// for the full reference.
package openclaw

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Identity is FleetCom's Ed25519 identity for one OpenClaw gateway.
// Each gateway we pair with gets its own keypair so that compromise of
// one host doesn't cascade. In production these come from agenix-mounted
// files at `/run/agenix/fleetcom-openclaw-<host>-{key,pubkey}`.
type Identity struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	// DeviceID is sha256(rawPubkeyBytes) hex — matches OpenClaw's
	// `fingerprintPublicKey` and is what the gateway uses as the device
	// primary key in `paired.json` and in pairing events.
	DeviceID string
	// PubKeyRawB64U is base64url-no-padding of the 32-byte raw Ed25519
	// public key. This is the exact format OpenClaw expects in the
	// connect frame's `device.publicKey` field.
	PubKeyRawB64U string
}

// LoadIdentity reads an Ed25519 PKCS#8 private key PEM (and optionally
// a matching PKIX SPKI public key PEM) from disk. If pubPath is empty,
// the public half is derived from the private key.
func LoadIdentity(privPath, pubPath string) (*Identity, error) {
	privBytes, err := os.ReadFile(privPath)
	if err != nil {
		return nil, fmt.Errorf("read priv: %w", err)
	}
	block, _ := pem.Decode(privBytes)
	if block == nil {
		return nil, errors.New("invalid private key PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}

	var pub ed25519.PublicKey
	if pubPath != "" {
		pubBytes, err := os.ReadFile(pubPath)
		if err != nil {
			return nil, fmt.Errorf("read pub: %w", err)
		}
		pblock, _ := pem.Decode(pubBytes)
		if pblock == nil {
			return nil, errors.New("invalid public key PEM")
		}
		pk, err := x509.ParsePKIXPublicKey(pblock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		pub, ok = pk.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("public key is not Ed25519")
		}
	} else {
		pub = priv.Public().(ed25519.PublicKey)
	}

	sum := sha256.Sum256(pub)
	return &Identity{
		PrivateKey:    priv,
		PublicKey:     pub,
		DeviceID:      hex.EncodeToString(sum[:]),
		PubKeyRawB64U: base64.RawURLEncoding.EncodeToString(pub),
	}, nil
}

// GenerateIdentity creates a fresh Ed25519 identity and persists it as
// PKCS#8/SPKI PEM files at the given paths. Intended for local
// development; production identities are generated via agenix.
func GenerateIdentity(privPath, pubPath string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(privPath), 0o755); err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return nil, err
	}
	return LoadIdentity(privPath, pubPath)
}

// Sign produces a base64url-no-padding Ed25519 signature over payload
// bytes, matching OpenClaw's `signDevicePayload`.
func (id *Identity) Sign(payload string) string {
	sig := ed25519.Sign(id.PrivateKey, []byte(payload))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// BuildPayloadV3 is the pipe-joined handshake payload that goes into
// the Ed25519 signature. This is an exact port of OpenClaw's
// `buildDeviceAuthPayloadV3`: any deviation (ordering, missing field,
// wrong normalization) will cause the gateway to reject the connect
// with `invalid-device-signature`.
func BuildPayloadV3(deviceID, clientID, clientMode, role string, scopes []string, signedAtMs int64, token, nonce, platform, deviceFamily string) string {
	return strings.Join([]string{
		"v3",
		deviceID,
		clientID,
		clientMode,
		role,
		strings.Join(scopes, ","),
		strconv.FormatInt(signedAtMs, 10),
		token,
		nonce,
		normalizeMetadataForAuth(platform),
		normalizeMetadataForAuth(deviceFamily),
	}, "|")
}

// FingerprintFromPubkeyPEM derives a gateway-compatible deviceId (full
// sha256 hex of the raw Ed25519 pubkey bytes) from a PKIX SPKI PEM. Used
// by the bridge-registration handler so `bridge_pairings.pubkey_fp` is
// in the same format the gateway emits in `device.pair.requested`
// events, making auto-approval a direct equality check.
func FingerprintFromPubkeyPEM(pubPEM string) (string, error) {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		return "", errors.New("invalid public key PEM")
	}
	pk, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	pub, ok := pk.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("public key is not Ed25519")
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:]), nil
}

// normalizeMetadataForAuth lowercases + trims, matching OpenClaw's
// `normalizeDeviceMetadataForAuth`. Empty input → empty string.
func normalizeMetadataForAuth(v string) string {
	t := strings.TrimSpace(v)
	if t == "" {
		return ""
	}
	return strings.ToLower(t)
}
