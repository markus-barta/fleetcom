package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
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

// RegisterBridgeRequest is the body of POST /api/bridges/register.
// Authentication is the host's bosun bearer token (shared with the
// bridge via env). The server derives the host from the token — the
// bridge cannot register itself under a different hostname.
type RegisterBridgeRequest struct {
	Agent     string `json:"agent"`      // e.g. "merlin"
	PubkeyPEM string `json:"pubkey_pem"` // Ed25519 SPKI PEM
}

// RegisterBridge handles POST /api/bridges/register. Bearer-authenticated
// by the host's bosun token; records the (host, agent, fingerprint)
// triple so the auto-approver can match it against pending pair requests
// seen on the host's OpenClaw gateway.
func RegisterBridge(store *db.Store, hub *sse.Hub) http.HandlerFunc {
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

		if err := store.RegisterBridge(hostname, body.Agent, fp, body.PubkeyPEM); err != nil {
			log.Printf("register bridge error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Broadcast bridges list update so the dashboard sees the new
		// row live.
		if bs, err := store.AllBridgePairings(); err == nil {
			if data, err := json.Marshal(bs); err == nil {
				hub.Broadcast("bridges", data)
			}
		}

		// Does the host have a paired gateway with auto-approve on? If
		// so, the (future) OpenClaw WS client will pick this up and
		// auto-approve. For now, flag it in the response so the bridge
		// knows whether to expect instant approval or manual pending.
		gws, _ := store.AllGateways()
		autoApprove := false
		for _, g := range gws {
			if g.Host == hostname && g.Status == "paired" && g.AutoApproveBridges {
				autoApprove = true
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"host":         hostname,
			"agent":        body.Agent,
			"fingerprint":  fp,
			"auto_approve": autoApprove,
			"status":       "registered",
		})
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

// ApproveBridge handles POST /api/bridges/{host}/{agent}/approve — admin only.
// Flips status='pending' → 'approved' and broadcasts SSE 'bridges'.
// Returns 404 when no pending row matches (already approved, revoked, etc).
func ApproveBridge(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		agent := chi.URLParam(r, "agent")
		if host == "" || agent == "" {
			http.Error(w, "host and agent required", http.StatusBadRequest)
			return
		}
		if err := store.MarkBridgeApprovedManual(host, agent); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
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

// SetGatewayAutoApprove toggles the per-gateway auto-approve flag.
func SetGatewayAutoApprove(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		mode := chi.URLParam(r, "mode") // "on" or "off"
		if host == "" || (mode != "on" && mode != "off") {
			http.Error(w, "host and mode (on|off) required", http.StatusBadRequest)
			return
		}
		if err := store.SetAutoApprove(host, mode == "on"); err != nil {
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
