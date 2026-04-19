package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

func Events(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Extend the write deadline for this long-lived SSE connection.
		// The server's global WriteTimeout (30s) would otherwise kill
		// the stream before the first keepalive fires.
		rc := http.NewResponseController(w)
		extendDeadline := func() {
			_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
		}
		extendDeadline()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// Determine user access scope
		u := auth.GetUser(r)
		isAdmin := u != nil && u.Role == "admin"

		// Send initial config
		cfgData, _ := json.Marshal(buildConfigPayload(store))
		fmt.Fprintf(w, "event: config\ndata: %s\n\n", cfgData)

		// Send initial ignored set (scoped to this user). Ignores are per-user,
		// so there's no hub broadcast — clients update local state after their
		// own POST/DELETE /api/ignore calls succeed.
		if u != nil {
			if ignoredSet, err := store.IgnoredSet(u.ID); err == nil {
				igData, _ := json.Marshal(ignoredSet)
				fmt.Fprintf(w, "event: ignored\ndata: %s\n\n", igData)
			}
		}

		// Send initial host configs (filtered)
		if hostCfgs, err := store.AllHostConfigs(); err == nil {
			if !isAdmin {
				hosts, _ := hostsForRequest(store, r)
				hostCfgs = filterHostConfigs(hostCfgs, hosts)
			}
			hcData, _ := json.Marshal(hostCfgs)
			fmt.Fprintf(w, "event: host-configs\ndata: %s\n\n", hcData)
		}

		// Send initial host state (filtered)
		hosts, err := hostsForRequest(store, r)
		if err != nil {
			log.Printf("SSE initial state error: %v", err)
		} else {
			data, _ := json.Marshal(hosts)
			fmt.Fprintf(w, "event: hosts\ndata: %s\n\n", data)
		}

		// Send initial agents list (filtered by host access)
		{
			hostIDs := make([]int64, 0, len(hosts))
			for _, h := range hosts {
				hostIDs = append(hostIDs, h.ID)
			}
			if summaries, err := store.ListAgentsForHosts(hostIDs); err == nil {
				data, _ := json.Marshal(summaries)
				fmt.Fprintf(w, "event: agents\ndata: %s\n\n", data)
			}
		}
		flusher.Flush()

		// Subscribe to updates
		ch := hub.Subscribe()
		defer hub.Unsubscribe(ch)

		// Keepalive ticker — prevents Cloudflare from killing idle connections
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case evt, ok := <-ch:
				if !ok {
					return
				}
				extendDeadline()
				// Filter host-related broadcasts for non-admin users
				if !isAdmin && (evt.Name == "hosts" || evt.Name == "host-configs" || evt.Name == "agents" || evt.Name == "agent-event") {
					filtered := filterSSEEvent(store, u, evt)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", filtered.Name, filtered.Data)
				} else {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Name, evt.Data)
				}
				flusher.Flush()
			case <-ticker.C:
				extendDeadline()
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}

// filterSSEEvent re-filters broadcast data for a specific user's access.
func filterSSEEvent(store *db.Store, u *db.User, evt sse.Event) sse.Event {
	if u == nil {
		return evt
	}

	switch evt.Name {
	case "hosts":
		// Re-query filtered hosts instead of filtering the broadcast payload
		hosts, err := store.HostsForUser(u.ID)
		if err != nil {
			return evt
		}
		data, _ := json.Marshal(hosts)
		return sse.Event{Name: "hosts", Data: data}

	case "host-configs":
		var cfgs map[string]db.HostConfig
		if err := json.Unmarshal(evt.Data, &cfgs); err != nil {
			return evt
		}
		hosts, _ := store.HostsForUser(u.ID)
		filtered := filterHostConfigs(cfgs, hosts)
		data, _ := json.Marshal(filtered)
		return sse.Event{Name: "host-configs", Data: data}

	case "agents":
		// Re-query filtered agents list for this user.
		hosts, err := store.HostsForUser(u.ID)
		if err != nil {
			return evt
		}
		hostIDs := make([]int64, 0, len(hosts))
		for _, h := range hosts {
			hostIDs = append(hostIDs, h.ID)
		}
		summaries, err := store.ListAgentsForHosts(hostIDs)
		if err != nil {
			return evt
		}
		data, _ := json.Marshal(summaries)
		return sse.Event{Name: "agents", Data: data}

	case "agent-event":
		// Drop events for hosts the user can't see.
		var ev db.AgentEvent
		if err := json.Unmarshal(evt.Data, &ev); err != nil {
			return evt
		}
		hosts, _ := store.HostsForUser(u.ID)
		allowed := false
		for _, h := range hosts {
			if h.Hostname == ev.Agent.Host {
				allowed = true
				break
			}
		}
		if !allowed {
			return sse.Event{Name: "agent-event", Data: []byte("null")}
		}
	}

	return evt
}
