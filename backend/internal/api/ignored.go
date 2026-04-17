package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
)

// ListIgnored returns the caller's ignored entities.
// GET /api/ignored
func ListIgnored(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		list, err := store.ListIgnored(u.ID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	}
}

// AddIgnore scopes to the caller and rejects entities on hosts they can't access.
// POST /api/ignore  body: {"entity_type":"host","entity_key":"dsc0"}
func AddIgnore(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			EntityType string `json:"entity_type"`
			EntityKey  string `json:"entity_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.EntityType == "" || body.EntityKey == "" {
			http.Error(w, "entity_type and entity_key required", http.StatusBadRequest)
			return
		}
		if body.EntityType != "host" && body.EntityType != "container" && body.EntityType != "agent" {
			http.Error(w, "entity_type must be host|container|agent", http.StatusBadRequest)
			return
		}
		if !userCanAccessEntity(store, u, body.EntityType, body.EntityKey) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := store.AddIgnored(u.ID, body.EntityType, body.EntityKey); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// RemoveIgnore scopes to the caller. Removing an entry for an entity the user no
// longer has access to is allowed (harmless cleanup).
// DELETE /api/ignore?entity_type=host&entity_key=dsc0
func RemoveIgnore(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		entityType := r.URL.Query().Get("entity_type")
		entityKey := r.URL.Query().Get("entity_key")
		if entityType == "" || entityKey == "" {
			http.Error(w, "entity_type and entity_key required", http.StatusBadRequest)
			return
		}
		if err := store.RemoveIgnored(u.ID, entityType, entityKey); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// hostnameFromEntityKey returns the hostname segment of an entity_key.
// host → "<hostname>", container/agent → "<hostname>/<name>".
func hostnameFromEntityKey(entityType, entityKey string) string {
	if entityType == "host" {
		return entityKey
	}
	if i := strings.IndexByte(entityKey, '/'); i > 0 {
		return entityKey[:i]
	}
	return entityKey
}

// userCanAccessEntity returns true if the user is an admin or the entity's host
// appears in the user's host-access list.
func userCanAccessEntity(store *db.Store, u *db.User, entityType, entityKey string) bool {
	if u == nil {
		return false
	}
	if u.Role == "admin" {
		return true
	}
	hostname := hostnameFromEntityKey(entityType, entityKey)
	if hostname == "" {
		return false
	}
	hosts, err := store.HostsForUser(u.ID)
	if err != nil {
		return false
	}
	for _, h := range hosts {
		if h.Hostname == hostname {
			return true
		}
	}
	return false
}
