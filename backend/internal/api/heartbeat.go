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
	Hostname      string             `json:"hostname"`
	OS            string             `json:"os"`
	Kernel        string             `json:"kernel"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	AgentVersion  string             `json:"agent_version"`
	Containers    []ContainerPayload `json:"containers"`
	Agents        []AgentPayload     `json:"agents"`
	// Hardware/metadata fields — all optional. Bosun sends HwStatic on
	// startup + on change, HwLive on every heartbeat once collection is
	// active, and Fastfetch only when it has been (re)run.
	HwStatic  *db.HwStatic    `json:"hw_static,omitempty"`
	HwLive    *db.HwLive      `json:"hw_live,omitempty"`
	Fastfetch json.RawMessage `json:"fastfetch_json,omitempty"`
	// Agent observability (FLEET-36) — bosun attaches one snapshot per
	// agent scraped from local exporters.
	AgentStates []db.AgentSnapshot `json:"agent_states,omitempty"`
}

type ContainerPayload struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	State        string `json:"state"`
	Health       string `json:"health"`
	RestartCount int    `json:"restart_count"`
	StartedAt    string `json:"started_at"`
	ExitCode     int    `json:"exit_code"`
	OOMKilled    bool   `json:"oom_killed"`
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

		hw := &db.HardwareHeartbeat{
			Static:    payload.HwStatic,
			Live:      payload.HwLive,
			Fastfetch: payload.Fastfetch,
		}
		command, err := store.UpsertHeartbeat(payload.Hostname, payload.OS, payload.Kernel, payload.UptimeSeconds, payload.AgentVersion, toDBContainers(payload.Containers), toDBAgents(payload.Agents), hw)
		if err != nil {
			log.Printf("heartbeat upsert error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Persist any agent snapshots attached to this heartbeat so the
		// read side has a canonical "latest" per agent without waiting
		// for events to catch up.
		for i := range payload.AgentStates {
			snap := payload.AgentStates[i]
			if snap.Name == "" {
				continue
			}
			// Override host from the token — exporter cannot claim another host.
			snap.Host = payload.Hostname
			if _, _, err := store.UpsertAgentSnapshot(payload.Hostname, snap); err != nil {
				log.Printf("agent snapshot upsert error: %v", err)
			}
		}

		// Broadcast to SSE clients
		hosts, err := store.AllHosts()
		if err != nil {
			log.Printf("heartbeat broadcast error: %v", err)
		} else {
			data, _ := json.Marshal(hosts)
			hub.Broadcast("hosts", data)
		}

		// Broadcast agents list too when snapshots arrived.
		if len(payload.AgentStates) > 0 {
			if ids, err := store.AllHostIDs(); err == nil {
				if summaries, err := store.ListAgentsForHosts(ids); err == nil {
					if data, err := json.Marshal(summaries); err == nil {
						hub.Broadcast("agents", data)
					}
				}
			}
		}

		// Return interval + optional command so agents can adapt and act.
		// Also piggyback any pending host_commands so bosun can dispatch
		// them without a second round-trip. PendingCommandsForHost flips
		// them to 'executing' atomically so each command is handed out
		// exactly once across heartbeats.
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"ok":       true,
			"interval": store.HeartbeatInterval(),
		}
		if command != "" {
			resp["command"] = command
		}
		if cmds, err := store.PendingCommandsForHost(hostname); err == nil && len(cmds) > 0 {
			resp["commands"] = cmds
		} else if err != nil {
			log.Printf("pending commands for %s: %v", hostname, err)
		}
		json.NewEncoder(w).Encode(resp)
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
		out[i] = db.Container{
			Name:         c.Name,
			Image:        c.Image,
			State:        c.State,
			Health:       c.Health,
			RestartCount: c.RestartCount,
			StartedAt:    c.StartedAt,
			ExitCode:     c.ExitCode,
			OOMKilled:    c.OOMKilled,
		}
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
