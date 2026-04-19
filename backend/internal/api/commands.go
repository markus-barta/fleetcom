package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

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
		if body.ID == 0 || (body.Status != "done" && body.Status != "failed") {
			http.Error(w, "id + status (done|failed) required", http.StatusBadRequest)
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cmds)
	}
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
func EnqueueCommand(store *db.Store) http.HandlerFunc {
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
		id, err := store.EnqueueCommand(host, body.Kind, paramsAny, uid)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "pending"})
	}
}
