package main

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Track whether we've already warned about missing watchtower config, so
// we don't spam logs on every heartbeat when the operator hasn't set it up.
var watchtowerMissingWarned sync.Once

// handleServerCommand acts on a single command string received in a
// heartbeat response. Unknown commands are logged and ignored so the
// server can roll out new verbs without breaking older agents.
func handleServerCommand(cmd string) {
	switch cmd {
	case "update":
		go triggerWatchtowerUpdate()
	default:
		log.Printf("unknown server command: %q (ignored)", cmd)
	}
}

// triggerWatchtowerUpdate calls the watchtower HTTP API to pull + recreate
// any container it manages (fleetcom-bosun carries the enable label).
// Fire-and-forget — never blocks the heartbeat loop.
func triggerWatchtowerUpdate() {
	baseURL := strings.TrimRight(os.Getenv("WATCHTOWER_URL"), "/")
	token := os.Getenv("WATCHTOWER_TOKEN")
	if baseURL == "" || token == "" {
		watchtowerMissingWarned.Do(func() {
			log.Printf("server requested update but WATCHTOWER_URL or WATCHTOWER_TOKEN is empty — ignoring (will not re-warn)")
		})
		return
	}

	log.Printf("server requested update — calling watchtower at %s/v1/update", baseURL)

	req, err := http.NewRequest("POST", baseURL+"/v1/update", bytes.NewReader(nil))
	if err != nil {
		log.Printf("watchtower request build error: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("watchtower call error: %v", err)
		return
	}
	defer resp.Body.Close()

	log.Printf("watchtower responded %d", resp.StatusCode)
}
