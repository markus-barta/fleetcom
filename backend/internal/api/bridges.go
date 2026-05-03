package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/openclaw"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// GatewayRevoker is the subset of the OpenClaw manager the revoke path
// needs — an interface so api tests can substitute a no-op.
type GatewayRevoker interface {
	RevokeBridgeOnGateway(ctx context.Context, host, deviceID string) error
}

// ConfirmationCodePusher is the FLEET-113 hook the OpenClaw manager
// implements. RegisterBridge calls it when the host's gateway has
// oob_delivery_enabled=ON to ship the freshly minted 6-digit code to
// the gateway so it can deliver it through the agent itself. Defined
// as an interface so tests can substitute a no-op.
type ConfirmationCodePusher interface {
	PushConfirmationCode(ctx context.Context, host, agent, code, fp string) error
}

// RegisterBridgeRequest is the body of POST /api/bridges/register.
// Authentication is the host's bosun bearer token (shared with the
// bridge via env). The server derives the host from the token — the
// bridge cannot register itself under a different hostname.
type RegisterBridgeRequest struct {
	Agent     string `json:"agent"`      // e.g. "merlin"
	PubkeyPEM string `json:"pubkey_pem"` // Ed25519 SPKI PEM
	// FLEET-114: Ed25519 signature over sha256(host || ":" || agent ||
	// ":" || pubkey_fp), produced by the gateway with its own private
	// key. b64-standard (not URL-safe) for compactness in JSON. Empty
	// string means "no attestation provided" — server policy decides
	// whether to reject (env=true) or pass-through with an audit row
	// (env=false).
	GatewaySignatureB64 string `json:"gateway_signature_b64,omitempty"`
}

// FLEET-114: env-gated global default + per-row outcome enum.
const (
	envAttestationRequired = "FLEETCOM_REGISTER_ATTESTATION_REQUIRED"

	AttestationStatusUnknown  = "unknown"
	AttestationStatusVerified = "verified"
	AttestationStatusSkipped  = "skipped"
)

// attestationGloballyRequired returns the env's setting. Default FALSE
// for the initial Phase-3 ship — strict enforcement requires the
// operator to first paste each gateway's Ed25519 pubkey via PUT
// /api/gateways/{host}/pubkey AND deploy bridges that include the
// gateway-signed body. Until both sides are wired up, enforcement
// would be a hard regression for existing installs.
//
// The end-state per FLEET-114 spec is default TRUE: a follow-up flips
// it once the operator-log audit shows zero ATTESTATION_SKIPPED rows
// for ≥2 weeks across all gateways. The env stays in place to let
// individual operators upgrade earlier.
func attestationGloballyRequired() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envAttestationRequired)))
	if v == "" {
		return false
	}
	return v == "true" || v == "1" || v == "on" || v == "yes"
}

// attestationCanonicalMessage is the bytes that get signed by the
// gateway and verified by the server. Format chosen to be stable, easy
// to reproduce in any language, and impossible to confuse with a
// neighboring field (`:` separator + sha256 wrapper).
func attestationCanonicalMessage(host, agent, fp string) []byte {
	h := sha256.Sum256([]byte(host + ":" + agent + ":" + fp))
	return h[:]
}

// verifyGatewayAttestation checks an Ed25519 signature against the
// canonical (host:agent:fp) message under the gateway's pubkey.
// Returns nil on valid signature, an error explaining the failure
// otherwise. Empty pubkey or empty sig → ErrAttestationMissing so the
// caller can route to the audit-skipped path.
func verifyGatewayAttestation(host, agent, fp, gatewayPubkeyB64, sigB64 string) error {
	if gatewayPubkeyB64 == "" {
		return errAttestationGatewayKeyUnknown
	}
	if sigB64 == "" {
		return errAttestationMissing
	}
	// Pubkey is stored in the OpenClaw raw-pubkey format (b64url-no-padding).
	pub, err := base64.RawURLEncoding.DecodeString(gatewayPubkeyB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid gateway pubkey: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature encoding")
	}
	if !ed25519.Verify(pub, attestationCanonicalMessage(host, agent, fp), sig) {
		return errAttestationInvalid
	}
	return nil
}

var (
	errAttestationMissing           = fmt.Errorf("gateway_attestation_missing")
	errAttestationInvalid           = fmt.Errorf("gateway_attestation_invalid")
	errAttestationGatewayKeyUnknown = fmt.Errorf("gateway_attestation_pubkey_unknown")
)

// FLEET-113 OOB-code parameters. Code TTL chosen to match Signal's
// safety-number model (operator window to read it on the agent and
// type it into FleetCom). Length is 6 digits → 1M space; combined
// with the 5-attempt rate limit gives an effective brute-force
// probability of 5/1M ≈ 5×10⁻⁶ per registration before auto-reject.
const (
	confirmationCodeTTL    = 5 * time.Minute
	confirmationCodeDigits = 6
)

// generateConfirmationCode returns a cryptographically random 6-digit
// code as a zero-padded string ("472819"). crypto/rand → rejection
// sampling via Int(reader, 10^digits) avoids the modulo-bias trap of
// `rand.Read` over a power-of-two byte slice.
func generateConfirmationCode() (string, error) {
	max := big.NewInt(1)
	for i := 0; i < confirmationCodeDigits; i++ {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", confirmationCodeDigits, n.Int64()), nil
}

// confirmationCodeHash returns hex(sha256(code || ":" || pubkey_fp)).
// The fingerprint salt binds the hash to a specific bridge so a leaked
// code cannot approve a different pending row (Signal-style salting).
func confirmationCodeHash(code, fp string) string {
	h := sha256.Sum256([]byte(code + ":" + fp))
	return hex.EncodeToString(h[:])
}

// gatewayOOBEnabled is a small helper to look up whether the host's
// gateway has oob_delivery_enabled=ON. Returns false on any miss
// (no gateway, store error) — caller treats that as "OOB not in
// effect" and falls back to the Phase-1 approve-without-code path.
func gatewayOOBEnabled(store *db.Store, host string) bool {
	gws, err := store.AllGateways()
	if err != nil {
		return false
	}
	for _, g := range gws {
		if g.Host == host {
			return g.OOBDeliveryEnabled
		}
	}
	return false
}

// RegisterBridge handles POST /api/bridges/register. Bearer-authenticated
// by the host's bosun token; records the (host, agent, fingerprint)
// triple so the auto-approver can match it against pending pair requests
// seen on the host's OpenClaw gateway.
//
// FLEET-113: when the host's gateway has oob_delivery_enabled=ON and the
// new row lands as status='pending', the server mints a 6-digit OOB code,
// stores SHA-256(code:fp), and pushes the plaintext to the gateway over
// its operator WS so the gateway can deliver it through the agent itself.
// pusher may be nil (e.g. in tests) — the code is still minted and stored
// so the operator can recover it via skip-oob if needed.
func RegisterBridge(store *db.Store, hub *sse.Hub, pusher ConfirmationCodePusher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractBearer(r)
		if token == "" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		hostname, err := store.ValidateToken(hashToken(token))
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		var body RegisterBridgeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		body.Agent = strings.TrimSpace(body.Agent)
		body.PubkeyPEM = strings.TrimSpace(body.PubkeyPEM)
		if body.Agent == "" || body.PubkeyPEM == "" {
			http.Error(w, "agent and pubkey_pem required", http.StatusBadRequest)
			return
		}

		// Fingerprint = sha256(raw Ed25519 pubkey bytes) hex — matches
		// OpenClaw's deviceId format, so the auto-approver can directly
		// equality-match pair.requested events against this row.
		fp, err := openclaw.FingerprintFromPubkeyPEM(body.PubkeyPEM)
		if err != nil {
			http.Error(w, "invalid pubkey_pem: "+err.Error(), http.StatusBadRequest)
			return
		}

		// FLEET-114: attestation gate runs BEFORE persisting the row so a
		// rejected registration leaves no trace. Pull the gateway's row
		// upfront — we need both its pubkey (for verify) and its
		// per-gateway flag (for the AND with the env).
		gws, gwErr := store.AllGateways()
		if gwErr != nil {
			log.Printf("warn: register bridge %s/%s: AllGateways failed: %v (proceeding as no-gateway)", hostname, body.Agent, gwErr)
		}
		var matchedGw *db.OpenClawGateway
		for i := range gws {
			if gws[i].Host == hostname {
				matchedGw = &gws[i]
				break
			}
		}
		gatewayPubkey := ""
		gatewayAttestationOn := true
		if matchedGw != nil {
			gatewayPubkey = matchedGw.GatewayPubkeyB64
			gatewayAttestationOn = matchedGw.AttestationRequired
		}
		// Effective enforcement = global env AND per-gateway flag. Either
		// can downgrade enforcement to "skip"; both must be ON to enforce.
		envOn := attestationGloballyRequired()
		enforce := envOn && gatewayAttestationOn

		attStatus := AttestationStatusUnknown
		var attSkipReason string
		if err := verifyGatewayAttestation(hostname, body.Agent, fp, gatewayPubkey, body.GatewaySignatureB64); err == nil {
			attStatus = AttestationStatusVerified
		} else {
			if enforce {
				log.Printf("register bridge %s/%s: attestation REJECT: %v", hostname, body.Agent, err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":  "gateway_attestation_required",
					"reason": err.Error(),
					"hint":   "set FLEETCOM_REGISTER_ATTESTATION_REQUIRED=false during rollout, or POST /api/gateways/{host}/attestation/off, or paste the gateway pubkey via /api/gateways/{host}/pubkey.",
				})
				return
			}
			attStatus = AttestationStatusSkipped
			attSkipReason = err.Error()
			log.Printf("register bridge %s/%s: attestation SKIPPED (%s)", hostname, body.Agent, attSkipReason)
		}

		if err := store.RegisterBridge(hostname, body.Agent, fp, body.PubkeyPEM); err != nil {
			log.Printf("register bridge error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Persist per-row attestation outcome so operators can audit.
		// Quietly no-ops on already-approved rows (FLEET-114 downgrade-protect filter).
		if err := store.SetBridgeAttestationStatus(hostname, body.Agent, attStatus); err != nil {
			log.Printf("warn: register bridge %s/%s: SetBridgeAttestationStatus(%q): %v", hostname, body.Agent, attStatus, err)
		}
		// FLEET-114: system audit row (user_id=0) so operators can grep
		// the operator log later for "what came in under skip during rollout?"
		if attStatus == AttestationStatusSkipped {
			_, _ = store.RecordActivity(db.ActivityEvent{
				UserID:     0,
				Verb:       "ATTESTATION_SKIPPED",
				TargetType: "bridge",
				TargetKey:  hostname + "/" + body.Agent,
				Outcome:    "ok",
				Error:      attSkipReason,
			})
		}

		// Broadcast bridges list update so the dashboard sees the new
		// row live.
		if bs, err := store.AllBridgePairings(); err == nil {
			if data, err := json.Marshal(bs); err == nil {
				hub.Broadcast("bridges", data)
			}
		}

		// matchedGw was pulled above for the attestation gate — re-use
		// it here for auto-approve + OOB.
		autoApprove := false
		oobEnabled := false
		if matchedGw != nil {
			if matchedGw.Status == "paired" && matchedGw.AutoApproveBridges {
				autoApprove = true
			}
			oobEnabled = matchedGw.OOBDeliveryEnabled
		}

		// FLEET-113: pending row + OOB-enabled gateway → mint a code,
		// store the salted hash, push plaintext to the gateway's WS for
		// agent-side delivery. We mint the code unconditionally on
		// pending so /approve-skip-oob remains the operator's recovery
		// path even if the WS push fails or the gateway hasn't shipped
		// the bridge.confirmation_code RPC yet.
		oobMinted := false
		if !autoApprove {
			code, codeErr := generateConfirmationCode()
			if codeErr != nil {
				log.Printf("warn: register bridge %s/%s: confirmation code mint failed: %v", hostname, body.Agent, codeErr)
			} else {
				expires := time.Now().UTC().Add(confirmationCodeTTL).Format(time.RFC3339)
				if err := store.SetConfirmationCode(hostname, body.Agent, confirmationCodeHash(code, fp), expires); err != nil {
					log.Printf("warn: register bridge %s/%s: persist confirmation code: %v", hostname, body.Agent, err)
				} else {
					oobMinted = true
				}
				if oobEnabled && pusher != nil {
					ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
					if err := pusher.PushConfirmationCode(ctx, hostname, body.Agent, code, fp); err != nil {
						log.Printf("warn: push confirmation code to gateway %s: %v", hostname, err)
					}
					cancel()
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                 true,
			"host":               hostname,
			"agent":              body.Agent,
			"fingerprint":        fp,
			"auto_approve":       autoApprove,
			"oob_required":       oobEnabled,
			"oob_minted":         oobMinted,
			"attestation_status": attStatus,
			"status":             "registered",
		})
	}
}

// SetGatewayAttestationRequired toggles the per-gateway attestation
// flag. URL: /api/gateways/{host}/attestation/{mode}.
func SetGatewayAttestationRequired(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		mode := chi.URLParam(r, "mode")
		if host == "" || (mode != "on" && mode != "off") {
			http.Error(w, "host and mode (on|off) required", http.StatusBadRequest)
			return
		}
		if err := store.SetGatewayAttestationRequired(host, mode == "on"); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if gs, err := store.AllGateways(); err == nil {
			if data, err := json.Marshal(gs); err == nil {
				hub.Broadcast("gateways", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// SetGatewayPubkeyRequest carries the operator-pasted pubkey. Format
// matches the rest of the OpenClaw stack: raw 32-byte Ed25519 pubkey
// encoded as base64url-no-padding (the same format as
// openclaw_gateways.fc_pubkey_b64).
type SetGatewayPubkeyRequest struct {
	PubkeyB64 string `json:"pubkey_b64"`
}

// SetGatewayPubkey lets an operator paste the gateway's own Ed25519
// pubkey so the server can verify attestation signatures from it.
// URL: PUT /api/gateways/{host}/pubkey. Empty string is allowed and
// resets the column (verification falls through to "skipped").
func SetGatewayPubkey(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		var body SetGatewayPubkeyRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		body.PubkeyB64 = strings.TrimSpace(body.PubkeyB64)
		// Validate format up-front — reject obviously-wrong values so
		// the operator gets immediate feedback rather than discovering
		// it via a broken signature on the next bridge registration.
		if body.PubkeyB64 != "" {
			raw, err := base64.RawURLEncoding.DecodeString(body.PubkeyB64)
			if err != nil || len(raw) != ed25519.PublicKeySize {
				http.Error(w, "pubkey must be base64url-no-padding of a 32-byte Ed25519 public key", http.StatusBadRequest)
				return
			}
		}
		if err := store.SetGatewayPubkey(host, body.PubkeyB64); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if gs, err := store.AllGateways(); err == nil {
			if data, err := json.Marshal(gs); err == nil {
				hub.Broadcast("gateways", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListBridges returns every bridge pairing row (admin).
func ListBridges(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bs, err := store.AllBridgePairings()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bs)
	}
}

// FLEET-112: pair-request approval surface.
//
// PendingBridgeView is the wire shape returned by GET /api/bridges/pending.
// fp_human is the SSH-style 8-byte fingerprint render (`a3:f1:9c:7d:4e:8b:2a:1f`)
// computed server-side so every consumer renders identically. gateway_status
// is `paired | unpaired | revoked | not_present` so the operator can spot
// "bridge registered but no gateway to endorse it" at a glance.
type PendingBridgeView struct {
	Host          string `json:"host"`
	Agent         string `json:"agent"`
	PubkeyFP      string `json:"pubkey_fp"`
	FpHuman       string `json:"fp_human"`
	CreatedAt     string `json:"created_at"`
	LastSeenAt    string `json:"last_seen_at"`
	GatewayStatus string `json:"gateway_status"`
}

// fpHumanShort renders the first 8 bytes of a hex fingerprint as
// colon-separated pairs. Same format as `ssh-keygen -lf` output and
// every other "TOFU first time you saw this key" UX in the world.
func fpHumanShort(hex string) string {
	if len(hex) < 16 {
		return hex
	}
	out := make([]byte, 0, 23)
	for i := 0; i < 16; i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[i], hex[i+1])
	}
	return string(out)
}

// ListPendingBridges handles GET /api/bridges/pending — admin only.
// Returns all rows with status='pending' enriched with fp_human +
// the host's gateway status so the UI can render advisory copy.
func ListPendingBridges(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pending, err := store.PendingBridges()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		gws, _ := store.AllGateways()
		gwStatus := map[string]string{}
		for _, g := range gws {
			gwStatus[g.Host] = g.Status
		}
		out := make([]PendingBridgeView, 0, len(pending))
		for _, b := range pending {
			st := gwStatus[b.Host]
			if st == "" {
				st = "not_present"
			}
			out = append(out, PendingBridgeView{
				Host:          b.Host,
				Agent:         b.Agent,
				PubkeyFP:      b.PubkeyFP,
				FpHuman:       fpHumanShort(b.PubkeyFP),
				CreatedAt:     b.LastSeenAt, // pending rows have no approved_at; last_seen_at = registration time
				LastSeenAt:    b.LastSeenAt,
				GatewayStatus: st,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// approveBridgeRequest is the optional body of /approve. FLEET-113
// adds the OOB code path; absent code triggers Phase-1 fallback only
// when the gateway has oob_delivery_enabled=OFF.
type approveBridgeRequest struct {
	Code string `json:"code"`
}

// ApproveBridge handles POST /api/bridges/{host}/{agent}/approve — admin only.
// FLEET-113 flow:
//
//   - Gateway oob_delivery_enabled=OFF → approve without code (Phase-1
//     behavior; lets ops land Phase 2 without breaking gateways that
//     haven't shipped the OpenClaw RFC yet).
//   - Gateway oob_delivery_enabled=ON, body has code → validate hash,
//     enforce TTL, enforce 5-attempt limit. Match → clear code, approve.
//     Mismatch → bump attempts, auto-reject + 410 at the limit.
//   - Gateway oob_delivery_enabled=ON, body missing code → 400 pointing
//     at /approve-skip-oob.
func ApproveBridge(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		agent := chi.URLParam(r, "agent")
		if host == "" || agent == "" {
			http.Error(w, "host and agent required", http.StatusBadRequest)
			return
		}

		var body approveBridgeRequest
		_ = json.NewDecoder(r.Body).Decode(&body) // body is optional; ignore decode error
		body.Code = strings.TrimSpace(body.Code)

		// Idempotency probe: SSE racing between approve clicks (or a
		// double-submit) can land here after the row has already flipped
		// to 'approved'. Treat the action as a successful no-op rather
		// than returning a confusing "no pending bridge" 404.
		if existing, _ := store.BridgeByHostAgent(host, agent); existing == nil {
			http.Error(w, "no bridge pairing for "+host+"/"+agent, http.StatusNotFound)
			return
		} else if existing.Status == "approved" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		oobOn := gatewayOOBEnabled(store, host)

		if oobOn {
			if body.Code == "" {
				http.Error(w, "OOB code required (or POST /approve-skip-oob with operator typed-confirm)", http.StatusBadRequest)
				return
			}
			cc, err := store.GetConfirmationCode(host, agent)
			if err != nil || cc == nil {
				http.Error(w, "no pending bridge pairing for "+host+"/"+agent, http.StatusNotFound)
				return
			}
			if cc.Hash == "" {
				http.Error(w, "no active confirmation code — wait for the bridge to re-register", http.StatusGone)
				return
			}
			if cc.ExpiresAt != "" {
				if t, perr := time.Parse(time.RFC3339, cc.ExpiresAt); perr == nil && time.Now().UTC().After(t) {
					http.Error(w, "confirmation code expired — wait for the bridge to re-register", http.StatusGone)
					return
				}
			}

			// QA-AUDIT-FIX: consume one brute-force slot ATOMICALLY before
			// the constant-time compare. Without this, N concurrent /approve
			// requests with N different bad codes would all read attempts<5,
			// all run the CTC (each a brute-force probe), and only afterward
			// increment — making the effective rate limit parallelism-bound
			// instead of 5/5. The DB-side `WHERE confirmation_attempts < ?`
			// clause is the serialization point.
			newAttempts, ok, err := store.ConsumeConfirmationAttempt(host, agent)
			if err != nil {
				log.Printf("warn: consume confirmation attempt %s/%s: %v", host, agent, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !ok {
				// Cap already reached, OR row gone — auto-reject either way.
				if err := store.DeleteBridgePairing(host, agent); err != nil {
					log.Printf("warn: delete bridge pairing %s/%s after cap reached: %v", host, agent, err)
				}
				broadcastBridges(store, hub)
				http.Error(w, "too many confirmation attempts — pairing auto-rejected, bridge must re-register", http.StatusGone)
				return
			}

			expected := confirmationCodeHash(body.Code, cc.PubkeyFP)
			if subtle.ConstantTimeCompare([]byte(expected), []byte(cc.Hash)) != 1 {
				// Mismatch — the slot has already been consumed. If that
				// consumption put us at the cap, auto-reject; else surface
				// the count to the operator as "X/N attempts".
				if newAttempts >= db.MaxConfirmationAttempts {
					if err := store.DeleteBridgePairing(host, agent); err != nil {
						log.Printf("warn: delete bridge pairing %s/%s at cap: %v", host, agent, err)
					}
					broadcastBridges(store, hub)
					http.Error(w, "too many confirmation attempts — pairing auto-rejected, bridge must re-register", http.StatusGone)
					return
				}
				http.Error(w, fmt.Sprintf("confirmation code mismatch (%d/%d attempts)", newAttempts, db.MaxConfirmationAttempts), http.StatusUnauthorized)
				return
			}
			// Match — clear the code (also resets the attempts counter)
			// so it cannot be replayed, then approve.
			if err := store.ClearConfirmationCode(host, agent); err != nil {
				log.Printf("warn: clear confirmation code %s/%s: %v", host, agent, err)
			}
		}

		if err := store.MarkBridgeApprovedManual(host, agent); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		broadcastBridges(store, hub)
		w.WriteHeader(http.StatusNoContent)
	}
}

// approveBridgeSkipOOBRequest requires the operator to echo the host
// name (typed-confirm at the API level — the UI already wraps this in
// confirmModal+requireType, but enforcing it server-side guarantees the
// audit row is loud even on direct curl).
type approveBridgeSkipOOBRequest struct {
	Confirm string `json:"confirm"`
}

// ApproveBridgeSkipOOB handles POST /api/bridges/{host}/{agent}/approve-skip-oob.
// Server-side typed-confirm: body must be `{"confirm":"<hostname>"}`.
// Always loud-audited at the SERVER level — the activity_events row is
// written here, not relied on the frontend's busy() recordOplog. That
// way curl-driven bypasses (intentional or scripted) are still captured
// in the audit trail. The frontend opts out of its own oplog write for
// this verb so the row appears exactly once.
func ApproveBridgeSkipOOB(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		agent := chi.URLParam(r, "agent")
		if host == "" || agent == "" {
			http.Error(w, "host and agent required", http.StatusBadRequest)
			return
		}
		var body approveBridgeSkipOOBRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Confirm) != host {
			http.Error(w, "typed-confirm mismatch — body.confirm must equal the hostname", http.StatusBadRequest)
			return
		}
		// Idempotency probe — same as ApproveBridge.
		if existing, _ := store.BridgeByHostAgent(host, agent); existing == nil {
			http.Error(w, "no bridge pairing for "+host+"/"+agent, http.StatusNotFound)
			return
		} else if existing.Status == "approved" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if err := store.MarkBridgeApprovedManual(host, agent); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = store.ClearConfirmationCode(host, agent)

		// Canonical audit row — written here regardless of whether the
		// caller is the dashboard or curl. user_id from the session
		// (admin middleware guarantees it's set).
		var userID int64
		if u := auth.GetUser(r); u != nil {
			userID = u.ID
		}
		// QA-AUDIT-FIX: log RecordActivity errors. The OOB_BYPASSED audit
		// row is the canonical loud-record for this loud action; a silent
		// failure would mean a security-relevant event went unrecorded.
		if _, err := store.RecordActivity(db.ActivityEvent{
			UserID:     userID,
			Verb:       "OOB_BYPASSED",
			TargetType: "bridge",
			TargetKey:  host + "/" + agent,
			Outcome:    "ok",
		}); err != nil {
			log.Printf("warn: OOB_BYPASSED audit-row write failed for %s/%s: %v", host, agent, err)
		}

		broadcastBridges(store, hub)
		w.WriteHeader(http.StatusNoContent)
	}
}

// SetGatewayOOBDelivery toggles the per-gateway oob_delivery_enabled flag.
// Mirrors SetGatewayAutoApprove. URL: /api/gateways/{host}/oob-delivery/{mode}.
//
// QA-AUDIT-FIX (PPM 1527): refuses to enable OOB when auto-approve is
// already ON. With both flags ON, RegisterBridge takes the auto-approve
// path and never mints/checks the OOB code — the operator's "I want OOB
// enforced" intent would be silently dropped. Better to reject the
// non-canonical combination at the toggle layer with an actionable hint.
// The posture cards (FLEET-117) already enforce this naturally — only
// the Advanced toggles disclosure can produce the bad state, and now
// it can't either.
func SetGatewayOOBDelivery(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		mode := chi.URLParam(r, "mode")
		if host == "" || (mode != "on" && mode != "off") {
			http.Error(w, "host and mode (on|off) required", http.StatusBadRequest)
			return
		}
		on := mode == "on"
		// Guard against the auto-approve + OOB silent override.
		if on {
			gws, err := store.AllGateways()
			if err == nil {
				for _, g := range gws {
					if g.Host == host && g.AutoApproveBridges {
						http.Error(w, "OOB enforcement requires auto-approve OFF — turn off auto-approve first (or pick the Reviewed/Hardened posture card)", http.StatusUnprocessableEntity)
						return
					}
				}
			}
		}
		if err := store.SetOOBDelivery(host, on); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if gs, err := store.AllGateways(); err == nil {
			if data, err := json.Marshal(gs); err == nil {
				hub.Broadcast("gateways", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// broadcastBridges is a tiny convenience for the approve/reject paths.
func broadcastBridges(store *db.Store, hub *sse.Hub) {
	if bs, err := store.AllBridgePairings(); err == nil {
		if data, err := json.Marshal(bs); err == nil {
			hub.Broadcast("bridges", data)
		}
	}
}

// RejectBridge handles POST /api/bridges/{host}/{agent}/reject — admin only.
// Hard-deletes the pending row (operator's signal: "this isn't mine").
// The bridge can re-register on its next attempt with a fresh fingerprint.
func RejectBridge(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		agent := chi.URLParam(r, "agent")
		if host == "" || agent == "" {
			http.Error(w, "host and agent required", http.StatusBadRequest)
			return
		}
		if err := store.DeleteBridgePairing(host, agent); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if bs, err := store.AllBridgePairings(); err == nil {
			if data, err := json.Marshal(bs); err == nil {
				hub.Broadcast("bridges", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// RevokeBridge drops the pairing row AND asks the gateway to revoke
// the operator token for that deviceId (via the OpenClaw WS client).
// Both steps are best-effort; the DB drop is what prevents future
// auto-approvals, the gateway revoke closes any active session. If the
// revoker isn't connected (gateway offline, FLEET-52 not deployed), the
// DB drop still takes effect.
func RevokeBridge(store *db.Store, hub *sse.Hub, revoker GatewayRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		agent := chi.URLParam(r, "agent")
		if host == "" || agent == "" {
			http.Error(w, "host and agent required", http.StatusBadRequest)
			return
		}
		bridge, _ := store.BridgeByHostAgent(host, agent)
		if err := store.DeleteBridgePairing(host, agent); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if revoker != nil && bridge != nil && bridge.PubkeyFP != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			if err := revoker.RevokeBridgeOnGateway(ctx, host, bridge.PubkeyFP); err != nil {
				log.Printf("gateway revoke failed for %s/%s: %v", host, agent, err)
			}
		}
		if bs, err := store.AllBridgePairings(); err == nil {
			if data, err := json.Marshal(bs); err == nil {
				hub.Broadcast("bridges", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListGateways returns every OpenClaw gateway row (admin).
func ListGateways(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gs, err := store.AllGateways()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gs)
	}
}

// SetGatewayPosture (FLEET-117) atomically applies a named posture
// (auto-pair / reviewed / hardened) to a gateway. One POST flips the
// three FLEET-111 flags to a canonical combination — the wizard
// surfaces this as a single posture-card click instead of three
// independent toggles.
//
// URL: POST /api/gateways/{host}/posture/{name}.
//
// Status codes:
//
//	204 — applied
//	400 — unknown posture name
//	404 — gateway row not found
//	422 — hardened requested but the gateway pubkey is empty (operator
//	      must paste it via PUT /api/gateways/{host}/pubkey first)
func SetGatewayPosture(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		name := chi.URLParam(r, "name")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		err := store.SetGatewayPosture(host, name)
		switch {
		case err == nil:
			// fallthrough to broadcast
		case errors.Is(err, db.ErrUnknownPosture):
			http.Error(w, "posture must be auto-pair, reviewed, or hardened", http.StatusBadRequest)
			return
		case errors.Is(err, db.ErrPostureLocked):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		default:
			// "gateway not found: <host>" or storage error
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if gs, err := store.AllGateways(); err == nil {
			if data, err := json.Marshal(gs); err == nil {
				hub.Broadcast("gateways", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// SetGatewayAutoApprove toggles the per-gateway auto-approve flag.
//
// QA-AUDIT-FIX (PPM 1527): symmetric guard with SetGatewayOOBDelivery —
// refuses to enable auto-approve when OOB is already ON. With both ON,
// RegisterBridge takes the auto-approve path and the OOB code is never
// minted/checked; better to reject the non-canonical combination here.
func SetGatewayAutoApprove(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		mode := chi.URLParam(r, "mode") // "on" or "off"
		if host == "" || (mode != "on" && mode != "off") {
			http.Error(w, "host and mode (on|off) required", http.StatusBadRequest)
			return
		}
		on := mode == "on"
		if on {
			gws, err := store.AllGateways()
			if err == nil {
				for _, g := range gws {
					if g.Host == host && g.OOBDeliveryEnabled {
						http.Error(w, "auto-approve cannot be enabled while OOB enforcement is ON — turn off OOB first (or pick the Auto-pair posture card)", http.StatusUnprocessableEntity)
						return
					}
				}
			}
		}
		if err := store.SetAutoApprove(host, on); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if gs, err := store.AllGateways(); err == nil {
			if data, err := json.Marshal(gs); err == nil {
				hub.Broadcast("gateways", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// requireAuthUser is a tiny helper so the handlers above don't repeat
// the auth.GetUser dance. Currently unused but kept for future manual-
// approval endpoints that need the admin's identity for audit.
var _ = auth.GetUser

// FLEET-109: bridge-deploy smart suggestion endpoint. The modal renders
// three additive chip rails — "ON THIS HOST", "SEEN IN YOUR FLEET", and
// "COMMON DEFAULTS" — populated server-side so the client doesn't have
// to load the whole fleet. Cached in-process for 60s per host.

// commonBridgeDefaults is the baseline set surfaced when the host has
// nothing paired and the fleet is empty. Hard-coded for v1; promote to
// a settings-table value if operators ever ask.
var commonBridgeDefaults = []string{"merlin", "nimue", "percival"}

type bridgeSuggestionsResponse struct {
	OnHost   []string `json:"on_host"`
	InFleet  []string `json:"in_fleet"`
	Defaults []string `json:"defaults"`
}

type bridgeSuggestionsCacheEntry struct {
	resp     bridgeSuggestionsResponse
	expires  time.Time
	hostname string
}

var (
	bridgeSuggestionsMu    sync.Mutex
	bridgeSuggestionsCache = map[string]bridgeSuggestionsCacheEntry{}
)

const bridgeSuggestionsTTL = 60 * time.Second

// BridgeSuggestions handles GET /api/bridges/suggestions/{host}.
// Admin-only (matches the rest of the bridge-management surface).
func BridgeSuggestions(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}

		bridgeSuggestionsMu.Lock()
		entry, ok := bridgeSuggestionsCache[host]
		bridgeSuggestionsMu.Unlock()
		if ok && time.Now().Before(entry.expires) {
			writeBridgeSuggestions(w, entry.resp)
			return
		}

		bridges, err := store.BridgeAgentsForHost(host)
		if err != nil {
			log.Printf("error: bridge suggestions / bridges for %s: %v", host, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		hb, err := store.HeartbeatAgentsForHost(host)
		if err != nil {
			log.Printf("error: bridge suggestions / heartbeat for %s: %v", host, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		fleet, err := store.TopBridgeNamesAcrossFleet(host, 3)
		if err != nil {
			log.Printf("error: bridge suggestions / fleet top: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Union bridges + heartbeat agents, preserving first-seen order.
		seen := map[string]bool{}
		onHost := []string{}
		for _, n := range bridges {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			onHost = append(onHost, n)
		}
		for _, n := range hb {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			onHost = append(onHost, n)
		}

		// in_fleet: exclude any name already on this host.
		inFleet := []string{}
		for _, n := range fleet {
			if seen[n] {
				continue
			}
			inFleet = append(inFleet, n)
		}

		// defaults: exclude anything already shown above.
		defaults := []string{}
		shown := map[string]bool{}
		for _, n := range onHost {
			shown[n] = true
		}
		for _, n := range inFleet {
			shown[n] = true
		}
		for _, n := range commonBridgeDefaults {
			if !shown[n] {
				defaults = append(defaults, n)
			}
		}

		resp := bridgeSuggestionsResponse{
			OnHost:   onHost,
			InFleet:  inFleet,
			Defaults: defaults,
		}

		bridgeSuggestionsMu.Lock()
		bridgeSuggestionsCache[host] = bridgeSuggestionsCacheEntry{
			resp:     resp,
			expires:  time.Now().Add(bridgeSuggestionsTTL),
			hostname: host,
		}
		bridgeSuggestionsMu.Unlock()

		writeBridgeSuggestions(w, resp)
	}
}

func writeBridgeSuggestions(w http.ResponseWriter, resp bridgeSuggestionsResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=30")
	_ = json.NewEncoder(w).Encode(resp)
}
