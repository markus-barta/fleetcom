package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// AgentSnapshot mirrors db.AgentSnapshot on the server — JSON-shaped
// pass-through; bosun doesn't introspect the fields.
type AgentSnapshot = json.RawMessage

// scrapeAgentStates fetches each URL in AGENT_STATE_URLS (comma-
// separated) or the single OPENCLAW_STATE_URL env var, and returns
// the union of their `agents: []` arrays. Missing env → empty slice.
//
// Failure mode: timeout 2s per URL, one log line on error, omit that
// source from the beat. The heartbeat never fails because of a broken
// exporter.
func scrapeAgentStates() []AgentSnapshot {
	urls := collectAgentStateURLs()
	if len(urls) == 0 {
		return nil
	}

	out := make([]AgentSnapshot, 0, 4)
	client := &http.Client{Timeout: 2 * time.Second}
	for _, url := range urls {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			log.Printf("agent-state: build req %s: %v", url, err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("agent-state: fetch %s: %v", url, err)
			continue
		}
		func() {
			defer func() {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
			if resp.StatusCode != 200 {
				log.Printf("agent-state: %s → HTTP %d", url, resp.StatusCode)
				return
			}
			var body struct {
				SchemaVersion int               `json:"schema_version"`
				Agents        []json.RawMessage `json:"agents"`
				// Also accept single-agent shorthand.
				Agent json.RawMessage `json:"agent,omitempty"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				log.Printf("agent-state: decode %s: %v", url, err)
				return
			}
			if len(body.Agent) > 0 {
				out = append(out, body.Agent)
			}
			out = append(out, body.Agents...)
		}()
	}
	return out
}

func collectAgentStateURLs() []string {
	seen := map[string]bool{}
	var urls []string
	add := func(v string) {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			urls = append(urls, p)
		}
	}
	add(os.Getenv("AGENT_STATE_URLS"))
	add(os.Getenv("OPENCLAW_STATE_URL"))
	return urls
}
