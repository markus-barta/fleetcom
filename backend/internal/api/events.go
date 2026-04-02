package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

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

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// Send initial state
		hosts, err := store.AllHosts()
		if err != nil {
			log.Printf("SSE initial state error: %v", err)
		} else {
			data, _ := json.Marshal(hosts)
			fmt.Fprintf(w, "event: hosts\ndata: %s\n\n", data)
			flusher.Flush()
		}

		// Subscribe to updates
		ch := hub.Subscribe()
		defer hub.Unsubscribe(ch)

		for {
			select {
			case <-r.Context().Done():
				return
			case data, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "event: hosts\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}
