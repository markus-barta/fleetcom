package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

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

		// Send initial config
		cfgData, _ := json.Marshal(buildConfigPayload(store))
		fmt.Fprintf(w, "event: config\ndata: %s\n\n", cfgData)

		// Send initial ignored set
		if ignoredSet, err := store.IgnoredSet(); err == nil {
			igData, _ := json.Marshal(ignoredSet)
			fmt.Fprintf(w, "event: ignored\ndata: %s\n\n", igData)
		}

		// Send initial host state
		hosts, err := store.AllHosts()
		if err != nil {
			log.Printf("SSE initial state error: %v", err)
		} else {
			data, _ := json.Marshal(hosts)
			fmt.Fprintf(w, "event: hosts\ndata: %s\n\n", data)
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
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Name, evt.Data)
				flusher.Flush()
			case <-ticker.C:
				extendDeadline()
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}
