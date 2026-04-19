package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// AgentEvents is the agent-push endpoint. Authenticated with the host's
// Bosun bearer token (same scope as /api/heartbeat). Exporter must
// include `agent: {host, name}` in the body; server validates that host
// matches the token's hostname, preventing cross-host impersonation.
func AgentEvents(store *db.Store, hub *sse.Hub) http.HandlerFunc {
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

		var body struct {
			Events []db.AgentEvent `json:"events"`
			// Allow single-event shorthand too.
			Event *db.AgentEvent `json:"event,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if body.Event != nil {
			body.Events = append(body.Events, *body.Event)
		}
		if len(body.Events) == 0 {
			http.Error(w, "no events in body", http.StatusBadRequest)
			return
		}

		accepted := 0
		for i := range body.Events {
			ev := &body.Events[i]
			// Scope guard: event's host must match the token's hostname.
			if ev.Agent.Host != hostname {
				log.Printf("agent-event rejected: host mismatch token=%s event=%s", hostname, ev.Agent.Host)
				continue
			}
			if err := store.InsertAgentEvent(*ev); err != nil {
				log.Printf("agent-event insert error: %v", err)
				continue
			}
			accepted++
			if data, err := json.Marshal(ev); err == nil {
				hub.Broadcast("agent-event", data)
			}
		}

		// If any event caused an agent row to materialise, re-broadcast
		// the agents list so dashboards pick up the new entity.
		hostIDs, _ := store.AllHostIDs()
		if summaries, err := store.ListAgentsForHosts(hostIDs); err == nil {
			if data, err := json.Marshal(summaries); err == nil {
				hub.Broadcast("agents", data)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"accepted": accepted,
		})
	}
}

// ListAgents returns agents visible to the caller.
func ListAgents(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := hostsForRequest(store, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		hostIDs := make([]int64, 0, len(hosts))
		for _, h := range hosts {
			hostIDs = append(hostIDs, h.ID)
		}
		out, err := store.ListAgentsForHosts(hostIDs)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// AgentDetail returns full detail for one agent.
func AgentDetail(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		name := chi.URLParam(r, "name")
		if host == "" || name == "" {
			http.Error(w, "host and name required", http.StatusBadRequest)
			return
		}
		// Access check: caller must have access to the host.
		hosts, err := hostsForRequest(store, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		ok := false
		for _, h := range hosts {
			if h.Hostname == host {
				ok = true
				break
			}
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		d, err := store.AgentDetail(host, name, 50)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if d == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d)
	}
}
