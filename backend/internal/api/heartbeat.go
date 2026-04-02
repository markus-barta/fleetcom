package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

type HeartbeatPayload struct {
	Hostname      string               `json:"hostname"`
	OS            string               `json:"os"`
	Kernel        string               `json:"kernel"`
	UptimeSeconds int64                `json:"uptime_seconds"`
	Containers    []ContainerPayload   `json:"containers"`
	Agents        []AgentPayload       `json:"agents"`
}

type ContainerPayload struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"`
}

type AgentPayload struct {
	Name      string `json:"name"`
	AgentType string `json:"agent_type"`
	Status    string `json:"status"`
}

func Heartbeat(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract and validate bearer token
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

		var payload HeartbeatPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		// Override hostname from token — agents can't impersonate other hosts
		payload.Hostname = hostname

		if err := store.UpsertHeartbeat(payload.Hostname, payload.OS, payload.Kernel, payload.UptimeSeconds, toDBContainers(payload.Containers), toDBAgents(payload.Agents)); err != nil {
			log.Printf("heartbeat upsert error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Broadcast to SSE clients
		hosts, err := store.AllHosts()
		if err != nil {
			log.Printf("heartbeat broadcast error: %v", err)
		} else {
			data, _ := json.Marshal(hosts)
			hub.Broadcast(data)
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(auth, "Bearer ")
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func toDBContainers(cs []ContainerPayload) []db.Container {
	out := make([]db.Container, len(cs))
	for i, c := range cs {
		out[i] = db.Container{Name: c.Name, Image: c.Image, State: c.State}
	}
	return out
}

func toDBAgents(as []AgentPayload) []db.Agent {
	out := make([]db.Agent, len(as))
	for i, a := range as {
		out[i] = db.Agent{Name: a.Name, AgentType: a.AgentType, Status: a.Status}
	}
	return out
}
