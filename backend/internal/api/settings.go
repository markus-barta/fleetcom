package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
	"github.com/markus-barta/fleetcom/internal/version"
)

// configPayload is the shape broadcast via SSE and returned by GET /api/settings.
type configPayload struct {
	HeartbeatInterval int    `json:"heartbeat_interval"`
	Commit            string `json:"commit"`
}

func buildConfigPayload(store *db.Store) configPayload {
	return configPayload{
		HeartbeatInterval: store.HeartbeatInterval(),
		Commit:            version.Commit,
	}
}

// BroadcastConfig pushes the current config to all SSE clients.
func BroadcastConfig(store *db.Store, hub *sse.Hub) {
	data, _ := json.Marshal(buildConfigPayload(store))
	hub.Broadcast("config", data)
}

// GetSettings returns the current server configuration.
// GET /api/settings (public — agents need the heartbeat interval).
func GetSettings(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildConfigPayload(store))
	}
}

// UpdateSettings accepts a partial config update (admin-only).
// PUT /api/settings
func UpdateSettings(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			HeartbeatInterval *int `json:"heartbeat_interval"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if body.HeartbeatInterval != nil {
			v := *body.HeartbeatInterval
			if v < 10 || v > 3600 {
				http.Error(w, "heartbeat_interval must be 10–3600", http.StatusBadRequest)
				return
			}
			if err := store.SetSetting("heartbeat_interval", strconv.Itoa(v)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		// Push new config to all connected browsers
		BroadcastConfig(store, hub)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildConfigPayload(store))
	}
}
