package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// RequestHostUpdate flags a single host for an auto-update on its next
// heartbeat. Admin-only. Returns 404 when the host doesn't exist.
func RequestHostUpdate(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := chi.URLParam(r, "hostname")
		if hostname == "" {
			http.Error(w, "hostname required", http.StatusBadRequest)
			return
		}
		ok, err := store.RequestUpdateByHostname(hostname)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "host not found", http.StatusNotFound)
			return
		}
		broadcastHostsUpdate(store, hub)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "hostname": hostname})
	}
}

// RequestUpdateAll flags every host in the caller's access scope.
func RequestUpdateAll(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		count, err := store.RequestUpdateAll(u.ID, u.Role == "admin")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		broadcastHostsUpdate(store, hub)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": count})
	}
}

// broadcastHostsUpdate re-sends the hosts list over SSE so any admin
// dashboards that are connected see the new update_requested_at flag
// immediately. Errors are logged via the normal SSE path when the
// broadcast can't be built.
func broadcastHostsUpdate(store *db.Store, hub *sse.Hub) {
	hosts, err := store.AllHosts()
	if err != nil {
		return
	}
	data, _ := json.Marshal(hosts)
	hub.Broadcast("hosts", data)
}
