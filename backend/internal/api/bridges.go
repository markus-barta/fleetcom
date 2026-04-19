package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
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
