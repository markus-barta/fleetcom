package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

type ContainerEventPayload struct {
	Hostname  string `json:"hostname"`
	Event     string `json:"event"`
	Container string `json:"container"`
	Image     string `json:"image"`
	ExitCode  int    `json:"exit_code"`
	OOMKilled bool   `json:"oom_killed"`
	Timestamp string `json:"timestamp"`
}

func ContainerEvents(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractBearer(r)
		if token == "" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}

		tokenHash := hashToken(token)
		hostname, err := store.ValidateToken(tokenHash)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		var payload ContainerEventPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		// Override hostname from token
		payload.Hostname = hostname

		if err := store.InsertContainerEvent(hostname, payload.Container, payload.Event, payload.ExitCode, payload.OOMKilled, payload.Timestamp); err != nil {
			log.Printf("container event insert error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Broadcast updated hosts to all SSE clients
		hosts, err := store.AllHosts()
		if err != nil {
			log.Printf("container event broadcast error: %v", err)
		} else {
			data, _ := json.Marshal(hosts)
			hub.Broadcast("hosts", data)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}
