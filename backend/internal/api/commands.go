package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/openclaw"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// destructiveCommandsEnabled reads FLEETCOM_DESTRUCTIVE_COMMANDS at request
// time so an operator can flip the gate without a server restart (env
// changes that compose-restart the container will still take effect on
// the next start; the lookup is cheap either way). Recognised truthy
// values: 1, true, yes, on (case-insensitive). Default: off.
func destructiveCommandsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FLEETCOM_DESTRUCTIVE_COMMANDS")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// commandResultRequest is the body bosun sends back after running a
// command it picked up from the heartbeat response.
type commandResultRequest struct {
	ID     int64           `json:"id"`
	Status string          `json:"status"` // "done" or "failed"
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// CommandResults handles POST /api/command-results. Bosun-authenticated
// (per-host bearer). We validate the command belongs to the caller's
// host so one compromised bosun can't write results for another host.
func CommandResults(store *db.Store, hub *sse.Hub) http.HandlerFunc {
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

		var body commandResultRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if body.ID == 0 || (body.Status != "done" && body.Status != "failed" && body.Status != "restarting") {
			http.Error(w, "id + status (done|failed|restarting) required", http.StatusBadRequest)
			return
		}

		resultJSON := ""
		if len(body.Result) > 0 {
			resultJSON = string(body.Result)
		}
		if err := store.MarkCommandResult(body.ID, hostname, body.Status, resultJSON, body.Error); err != nil {
			log.Printf("command result %d for %s: %v", body.ID, hostname, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Broadcast so the dashboard history view updates live.
		if cmds, err := store.CommandsForHost(hostname, 50); err == nil {
			for i := range cmds {
				cmds[i].Params = redactCommandParams(cmds[i].Kind, cmds[i].Params)
			}
			if data, err := json.Marshal(map[string]any{"host": hostname, "commands": cmds}); err == nil {
				hub.Broadcast("commands", data)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListCommands returns the most recent commands for one host. Admin-only.
func ListCommands(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		limit := 100
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		cmds, err := store.CommandsForHost(host, limit)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Redact sensitive fields from params before surfacing to the
		// admin UI. Keeps audit info (what kind, what container name,
		// what agent names) while scrubbing shared secrets like the
		// operator_token that rides in openclaw.pair params.
		for i := range cmds {
			cmds[i].Params = redactCommandParams(cmds[i].Kind, cmds[i].Params)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cmds)
	}
}

// redactCommandParams strips known-sensitive fields per kind so the
// admin UI can show params for audit without leaking secrets that were
// only ever meant for bosun. Returns the original payload untouched
// for unknown kinds (principle: fail safe on surface area, not on
// payload shape).
func redactCommandParams(kind string, params json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return params
	}
	sensitive := map[string][]string{
		"openclaw.pair":    {"operator_token"},
		"bridge.install":   {"fleetcom_token", "gateway_operator_token"},
		"bridge.reinstall": {"fleetcom_token", "gateway_operator_token"},
	}
	fields, ok := sensitive[kind]
	if !ok {
		return params
	}
	var m map[string]interface{}
	if err := json.Unmarshal(params, &m); err != nil {
		return params
	}
	for _, f := range fields {
		if _, present := m[f]; present {
			m[f] = "***redacted***"
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return params
	}
	return out
}

// CancelCommand drops a pending command before bosun picks it up.
// Once a command has been handed out on a heartbeat it's in flight and
// can't be unissued — admin has to wait for the result.
func CancelCommand(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if err := store.CancelCommand(id); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// EnqueueCommandRequest is the admin-side body for POST /api/hosts/{host}/commands.
// kind must appear in bosun's compiled-in allowlist or the command will
// fail fast on the bosun side.
type EnqueueCommandRequest struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params"`
}

// EnqueueCommand is the admin endpoint to issue a new command for a
// host. Status recording (issued_by_user_id) comes from the session.
// ocMgr is consulted for bridge.install to inject the gateway shared
// secret (FLEET-129) — pass nil if openclaw integration is disabled
// and the bridge will run in unauthenticated mode (gateway must be
// configured without auth.token in that case).
func EnqueueCommand(store *db.Store, ocMgr *openclaw.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		var body EnqueueCommandRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kind == "" {
			http.Error(w, "kind required", http.StatusBadRequest)
			return
		}
		user := auth.GetUser(r)
		var uid *int64
		if user != nil {
			v := user.ID
			uid = &v
		}

		var paramsAny interface{}
		if len(body.Params) > 0 {
			if err := json.Unmarshal(body.Params, &paramsAny); err != nil {
				http.Error(w, "invalid params json", http.StatusBadRequest)
				return
			}
		}

		// FLEET-369.1: host.reboot is destructive — gate hard.
		if body.Kind == "host.reboot" {
			if !destructiveCommandsEnabled() {
				http.Error(w, "host.reboot is disabled on this server (set FLEETCOM_DESTRUCTIVE_COMMANDS=1 to enable)", http.StatusForbidden)
				return
			}
			allow, err := store.AllowRebootForHost(host)
			if err != nil {
				log.Printf("host.reboot pre-flight (allow_reboot) for %s: %v", host, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !allow {
				http.Error(w, fmt.Sprintf("host %s: allow_reboot is off (per-host kill switch). Toggle via Settings.", host), http.StatusForbidden)
				return
			}
			if existing, err := store.PendingOrInflightHostReboot(host); err == nil && existing != 0 {
				http.Error(w, fmt.Sprintf("host.reboot %d already in progress for %s", existing, host), http.StatusConflict)
				return
			} else if err != nil {
				log.Printf("host.reboot pre-flight (idempotency) for %s: %v", host, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			// Capture pre-reboot boot_id so the heartbeat handler can
			// reconcile post-reboot. Empty is fine — the reconcile is
			// a no-op until both sides know a value.
			preBoot, _ := store.BootIDForHost(host)
			pmap, _ := paramsAny.(map[string]interface{})
			if pmap == nil {
				pmap = map[string]interface{}{}
			}
			pmap["pre_reboot_boot_id"] = preBoot
			paramsAny = pmap
		}

		// FLEET-85: agent.update has special pre-flight + capture logic.
		// We need the host's deployment shape to know whether bosun can
		// remote-update at all, and the current agent_version so we can
		// reconcile the post-restart heartbeat that confirms success.
		if body.Kind == "agent.update" {
			shape, err := store.DeploymentShapeForHost(host)
			if err != nil {
				log.Printf("agent.update pre-flight (shape) for %s: %v", host, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if shape == "" || shape == "unknown" {
				http.Error(w, fmt.Sprintf("host %s: deployment_shape is %q — bosun cannot self-update remotely; manual update required", host, shape), http.StatusBadRequest)
				return
			}
			if existing, err := store.PendingOrInflightAgentUpdate(host); err == nil && existing != 0 {
				http.Error(w, fmt.Sprintf("agent.update %d already in progress for %s", existing, host), http.StatusConflict)
				return
			} else if err != nil {
				log.Printf("agent.update pre-flight (idempotency) for %s: %v", host, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			// Capture pre-update agent_version so the heartbeat handler
			// can reconcile post-restart. Default empty if unknown.
			preVer, _ := store.AgentVersionForHost(host)
			pmap, _ := paramsAny.(map[string]interface{})
			if pmap == nil {
				pmap = map[string]interface{}{}
			}
			pmap["pre_update_version"] = preVer
			if _, ok := pmap["target"]; !ok {
				pmap["target"] = "latest"
			}
			if _, ok := pmap["source"]; !ok {
				pmap["source"] = "ghcr"
			}
			paramsAny = pmap
		}

		// FLEET-134: bridge auth uses the gateway's own shared-secret,
		// bind-mounted on the bridge host by bosun (not relayed through
		// the command queue). FLEET-129's relay was the wrong secret —
		// FleetCom's operator-token is a different artefact from the
		// gateway's auth.token. Removed; the gateway_operator_token
		// param is left wired in bosun's bridgeInstallParams as a
		// dev/test fallback override but is no longer populated server-
		// side.

		id, err := store.EnqueueCommand(host, body.Kind, paramsAny, uid)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "pending"})
	}
}
