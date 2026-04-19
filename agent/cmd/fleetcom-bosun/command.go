package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// ---------- FLEET-60: host command channel ----------

// hostCommand is the envelope FleetCom sends down in heartbeat responses.
// Params are deliberately opaque — each kind decodes its own shape.
type hostCommand struct {
	ID     int64           `json:"id"`
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params"`
}

// commandResult is what we POST back to /api/command-results.
type commandResult struct {
	ID     int64           `json:"id"`
	Status string          `json:"status"` // "done" or "failed"
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// commandHandler executes one command and returns (resultJSON, error).
// resultJSON is opaque, surfaced to admins in the command history.
// A nil error means status=done; any returned error means status=failed.
type commandHandler func(params json.RawMessage) (json.RawMessage, error)

// commandAllowlist is bosun's compiled-in set of commands it will run.
// Unknown kinds fail fast so a compromised / buggy server can't coax
// bosun into arbitrary behaviour. New handlers land by adding entries
// here in a subsequent FLEET-6x ticket.
var commandAllowlist = map[string]commandHandler{
	"openclaw.pair":     handleOpenclawPair,     // FLEET-61
	"container.restart": handleContainerRestart, // FLEET-62
	"bridge.install":    handleBridgeInstall,    // FLEET-63
}

// dispatchCommands is called once per heartbeat with the commands the
// server just handed out. Each runs in its own goroutine so a slow
// handler doesn't stall the heartbeat loop. Results are POSTed back
// independently.
func dispatchCommands(cmds []hostCommand, serverURL, token string) {
	for _, c := range cmds {
		go runAndReport(c, serverURL, token)
	}
}

func runAndReport(c hostCommand, serverURL, token string) {
	handler, ok := commandAllowlist[c.Kind]
	if !ok {
		log.Printf("command %d: unknown kind %q — rejecting", c.ID, c.Kind)
		reportResult(serverURL, token, commandResult{
			ID:     c.ID,
			Status: "failed",
			Error:  fmt.Sprintf("unknown command kind: %s", c.Kind),
		})
		return
	}

	log.Printf("command %d: running %s", c.ID, c.Kind)
	// Defensive timeout so a stuck handler can't hold up the report.
	// Each handler should enforce its own finer-grained deadline too.
	done := make(chan commandResult, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("command %d: handler panicked: %v", c.ID, p)
				done <- commandResult{ID: c.ID, Status: "failed", Error: fmt.Sprintf("handler panic: %v", p)}
			}
		}()
		result, err := handler(c.Params)
		if err != nil {
			done <- commandResult{ID: c.ID, Status: "failed", Result: result, Error: err.Error()}
			return
		}
		done <- commandResult{ID: c.ID, Status: "done", Result: result}
	}()

	select {
	case r := <-done:
		reportResult(serverURL, token, r)
	case <-time.After(4 * time.Minute):
		// Server-side expiry is 5m; we fail-report just before so the
		// admin sees a clean bosun-reported error rather than a server
		// timeout.
		log.Printf("command %d: handler timed out", c.ID)
		reportResult(serverURL, token, commandResult{
			ID:     c.ID,
			Status: "failed",
			Error:  "bosun handler timeout after 4m",
		})
	}
}

// reportResult POSTs one command's outcome to FleetCom. Retries once
// on transient failure; drops otherwise (server-side timeout will
// eventually mark the command as stuck).
func reportResult(serverURL, token string, r commandResult) {
	body, err := json.Marshal(r)
	if err != nil {
		log.Printf("marshal command result: %v", err)
		return
	}
	url := strings.TrimRight(serverURL, "/") + "/api/command-results"
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := doPost(url, token, body); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("command %d: result POST failed after retries — server will time it out", r.ID)
}
