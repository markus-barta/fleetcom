package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

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

		// Fingerprint = first 16 bytes of sha256(pubkey_pem), hex. This
		// is what the gateway's paired.json keys the device by (modulo
		// exact algorithm — to be reconciled with OpenClaw's deviceId
		// scheme when the WS client lands).
		h := sha256.Sum256([]byte(body.PubkeyPEM))
		fp := hex.EncodeToString(h[:16])

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

// RevokeBridge drops the pairing row. The actual gateway-side
// `device.token.revoke` call happens in the OpenClaw WS client — not
// wired yet, tracked in FLEET-51 task list. Removing the row prevents
// future auto-approvals of the same fingerprint.
func RevokeBridge(store *db.Store, hub *sse.Hub) http.HandlerFunc {
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
