package api

import (
	"encoding/json"
	"net/http"

	"github.com/markus-barta/fleetcom/internal/db"
)

type historyResponse struct {
	EntityType  string      `json:"entity_type"`
	EntityKey   string      `json:"entity_key"`
	Scale       string      `json:"scale"`
	Window      int64       `json:"window_seconds"`
	FirstSample string      `json:"first_sample"`
	Buckets     []db.Bucket `json:"buckets"`
}

// History returns bucketed status samples for a single entity+scale.
// GET /api/history?entity_type=host&entity_key=csb1&scale=1h
func History(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entityType := r.URL.Query().Get("entity_type")
		entityKey := r.URL.Query().Get("entity_key")
		scaleName := r.URL.Query().Get("scale")

		if entityType == "" || entityKey == "" || scaleName == "" {
			http.Error(w, "entity_type, entity_key, scale required", http.StatusBadRequest)
			return
		}
		if entityType != "host" && entityType != "container" && entityType != "agent" {
			http.Error(w, "entity_type must be host|container|agent", http.StatusBadRequest)
			return
		}

		scale, ok := db.FindScale(scaleName)
		if !ok {
			http.Error(w, "unknown scale", http.StatusBadRequest)
			return
		}

		buckets, err := store.HistoryBuckets(entityType, entityKey, scale)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		firstSample, err := store.EntityFirstSample(entityType, entityKey)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(historyResponse{
			EntityType:  entityType,
			EntityKey:   entityKey,
			Scale:       scaleName,
			Window:      int64(scale.Window.Seconds()),
			FirstSample: firstSample,
			Buckets:     buckets,
		})
	}
}
