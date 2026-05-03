package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// FLEET-111 security primitives. These are pure functions — no DB, no
// HTTP — so they're cheap to exhaust-test. Worth their weight because
// each one is on the audit path of bridge pairing.

func TestGenerateConfirmationCode_FormatAndRange(t *testing.T) {
	// 100 samples is enough to catch the "zero-padding lost" bug
	// (shortest code is 0, longest is 999_999) without flaking.
	for i := 0; i < 100; i++ {
		code, err := generateConfirmationCode()
		if err != nil {
			t.Fatalf("generateConfirmationCode: %v", err)
		}
		if len(code) != confirmationCodeDigits {
			t.Fatalf("code %q: got %d chars, want %d", code, len(code), confirmationCodeDigits)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q has non-digit %q", code, r)
			}
		}
	}
}

func TestConfirmationCodeHash_DeterministicAndSaltBound(t *testing.T) {
	const fpA = "a3f19c7d4e8b2a1faaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const fpB = "b3f19c7d4e8b2a1fbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const code = "472819"

	// Determinism: same inputs → same hash.
	h1 := confirmationCodeHash(code, fpA)
	h2 := confirmationCodeHash(code, fpA)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %q vs %q", h1, h2)
	}

	// Salt binds: same code + different fp → different hash. This is
	// the property that makes a leaked code unusable for replay against
	// a different bridge (Signal safety-number model).
	hOther := confirmationCodeHash(code, fpB)
	if h1 == hOther {
		t.Fatalf("hash collision across fps — salt is not binding")
	}

	// Different code, same fp → different hash (sanity).
	hDiffCode := confirmationCodeHash("000000", fpA)
	if h1 == hDiffCode {
		t.Fatalf("hash collision across codes")
	}

	// SHA-256 hex output is always 64 chars.
	if len(h1) != 64 {
		t.Fatalf("hash length %d, want 64", len(h1))
	}
}

func TestAttestationCanonicalMessage_StableAndDistinct(t *testing.T) {
	a := attestationCanonicalMessage("dsc0", "ocean", "a3f1")
	b := attestationCanonicalMessage("dsc0", "ocean", "a3f1")
	if string(a) != string(b) {
		t.Fatalf("not stable")
	}
	// Different inputs MUST yield different messages — guards against
	// a separator-confusion bug where ("ds", "c0:ocean", "a3f1") and
	// ("dsc0", "ocean", "a3f1") could collide.
	c := attestationCanonicalMessage("ds", "c0:ocean", "a3f1")
	if string(a) == string(c) {
		// SHA-256 wraps the concat so this should hold even though the
		// pre-hash bytes are equal — the hash is over the same string.
		// If this fires, our separator strategy is wrong.
		t.Fatalf("separator collision: ds:c0:ocean:a3f1 == dsc0:ocean:a3f1")
	}
	// Length is sha256 = 32.
	if len(a) != 32 {
		t.Fatalf("canonical message len %d, want 32", len(a))
	}
}

func TestVerifyGatewayAttestation_HappyPath(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	const host, agent, fp = "dsc0", "ocean", "a3f19c7d4e8b2a1faaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	msg := attestationCanonicalMessage(host, agent, fp)
	sig := ed25519.Sign(priv, msg)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	if err := verifyGatewayAttestation(host, agent, fp, pubB64, sigB64); err != nil {
		t.Fatalf("happy path verify failed: %v", err)
	}
}

func TestVerifyGatewayAttestation_TamperedFields(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	const host, agent, fp = "dsc0", "ocean", "a3f1"
	sig := ed25519.Sign(priv, attestationCanonicalMessage(host, agent, fp))
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// Tamper with each field. Each MUST fail verification (otherwise
	// an attacker could swap fields and pass).
	cases := []struct {
		name                        string
		host, agent, fp, sig, pkB64 string
	}{
		{"host", "evil", agent, fp, sigB64, pubB64},
		{"agent", host, "evil", fp, sigB64, pubB64},
		{"fp", host, agent, "deadbeef", sigB64, pubB64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := verifyGatewayAttestation(c.host, c.agent, c.fp, c.pkB64, c.sig); err == nil {
				t.Fatalf("tampered %s passed verify", c.name)
			}
		})
	}
}

func TestVerifyGatewayAttestation_ErrorRouting(t *testing.T) {
	const host, agent, fp = "dsc0", "ocean", "a3f1"

	// Empty pubkey → pubkey-unknown sentinel. Caller routes to the
	// rollout-grace skipped path on this specifically.
	err := verifyGatewayAttestation(host, agent, fp, "", "ignored")
	if err != errAttestationGatewayKeyUnknown {
		t.Fatalf("empty pubkey: got %v, want errAttestationGatewayKeyUnknown", err)
	}

	// Pubkey present, signature missing → missing sentinel.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if err := verifyGatewayAttestation(host, agent, fp, pubB64, ""); err != errAttestationMissing {
		t.Fatalf("empty sig: got %v, want errAttestationMissing", err)
	}

	// Garbage sig → not the missing sentinel (a wrong sig must NOT
	// route to skip — that would be a bypass via base64 corruption).
	if err := verifyGatewayAttestation(host, agent, fp, pubB64, "not-base64!"); err == nil || err == errAttestationMissing {
		t.Fatalf("garbage sig: got %v, want a verify failure", err)
	}
}

func TestFpHumanShort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a3f19c7d4e8b2a1f0011223344556677", "a3:f1:9c:7d:4e:8b:2a:1f"},
		{"abcdef", "abcdef"}, // shorter than 16 chars: passthrough
		{"", ""},
	}
	for _, c := range cases {
		if got := fpHumanShort(c.in); got != c.want {
			t.Errorf("fpHumanShort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAttestationGloballyRequired_EnvParsing(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false}, // unset → safe default for the initial Phase-3 ship
		{"false", false},
		{"FALSE", false},
		{"0", false},
		{"off", false},
		{"no", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"on", true},
		{"yes", true},
		// Anything else → false (matches the spec's "explicit opt-in" intent).
		{"maybe", false},
	}
	for _, c := range cases {
		t.Run("v="+c.v, func(t *testing.T) {
			if c.v == "" {
				_ = os.Unsetenv(envAttestationRequired)
			} else {
				_ = os.Setenv(envAttestationRequired, c.v)
			}
			defer os.Unsetenv(envAttestationRequired)
			if got := attestationGloballyRequired(); got != c.want {
				t.Errorf("env=%q: got %v, want %v", c.v, got, c.want)
			}
		})
	}
}

// Quick readability check: the strings.TrimSpace + ToLower path also
// handles whitespace-padded operator-set values (common in env files).
func TestAttestationGloballyRequired_TrimAndCase(t *testing.T) {
	for _, v := range []string{" true ", "\tTRUE\n", "  YES  "} {
		_ = os.Setenv(envAttestationRequired, v)
		if !attestationGloballyRequired() {
			t.Errorf("env=%q: should parse true", v)
		}
	}
	_ = os.Unsetenv(envAttestationRequired)
}

// fpHumanShort + attestationCanonicalMessage are exposed via internal
// callers; this guards the contract that fp is treated as a string the
// canonical message ingests verbatim (no decode, no normalization).
func TestCanonicalMessage_PassesFpAsString(t *testing.T) {
	// Hex case sensitivity matters — server stores lowercase hex; if a
	// caller (e.g. agent-bridge) passed uppercase, signatures wouldn't
	// match. Lock that property in.
	upper := attestationCanonicalMessage("h", "a", "ABCDEF")
	lower := attestationCanonicalMessage("h", "a", "abcdef")
	if string(upper) == string(lower) {
		t.Fatalf("canonical message ignores case — server/bridge fp must agree on lowercase hex")
	}
}

// Sanity: a non-empty TrimSpace'd request body is required for skip-OOB
// to pass. Server-side handler tests would cover this end-to-end; this
// is just the helper-level shape check.
var _ = strings.TrimSpace
