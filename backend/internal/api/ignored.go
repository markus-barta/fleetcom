package api

import (
	"encoding/json"
	"net/http"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

func broadcastIgnored(store *db.Store, hub *sse.Hub) {
	set, err := store.IgnoredSet()
	if err != nil {
		return
	}
	data, _ := json.Marshal(set)
	hub.Broadcast("ignored", data)
}

// ListIgnored returns all ignored entities.
// GET /api/ignored
func ListIgnored(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ListIgnored()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	}
}

// AddIgnore adds an entity to the ignore list.
// POST /api/ignore  body: {"entity_type":"host","entity_key":"dsc0"}
func AddIgnore(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if err := store.AddIgnored(body.EntityType, body.EntityKey); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		broadcastIgnored(store, hub)
		w.WriteHeader(http.StatusNoContent)
	}
}

// RemoveIgnore removes an entity from the ignore list.
// DELETE /api/ignore?entity_type=host&entity_key=dsc0
func RemoveIgnore(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entityType := r.URL.Query().Get("entity_type")
		entityKey := r.URL.Query().Get("entity_key")
		if entityType == "" || entityKey == "" {
			http.Error(w, "entity_type and entity_key required", http.StatusBadRequest)
			return
		}
		if err := store.RemoveIgnored(entityType, entityKey); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		broadcastIgnored(store, hub)
		w.WriteHeader(http.StatusNoContent)
	}
}
