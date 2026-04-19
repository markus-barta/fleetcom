// agent-bridge is the reference FleetCom agent exporter. It sits
// next to OpenClaw (or any other agent runtime) on the host, tails
// docker logs for structured events, maintains in-memory per-agent
// state, serves GET /v1/agent-state for Bosun to scrape, and pushes
// lifecycle events to FleetCom's POST /api/agent-events endpoint.
//
// Schema: see fleetcom/docs/AGENT-OBSERVABILITY.md
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "0.1.0"

func main() {
	cfg := loadConfig()
	log.Printf("fleetcom agent-bridge %s starting: fleetcom=%s container=%s bind=%s agents=%v",
		version, cfg.FleetcomURL, cfg.LogContainer, cfg.BindAddr, cfg.AgentNames)

	state := newState(cfg.AgentNames, cfg.AgentType)
	emitter := newEmitter(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shut down cleanly on SIGTERM — flush an abandon event for any
	// in-flight turns so the dashboard clears the zombies.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("shutdown: abandoning in-flight turns")
		for _, ev := range state.abandonInFlight() {
			emitter.send(ev)
		}
		cancel()
	}()

	// Start the log tailer (the sole event source in this MVP).
	go tailDockerLogs(ctx, cfg.LogContainer, state, emitter)

	// HTTP server: /v1/agent-state (pull by Bosun) + /healthz.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent-state", func(w http.ResponseWriter, r *http.Request) {
		snap := state.snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": 1,
			"agents":         snap,
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{
		Addr:         cfg.BindAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("listening on %s", cfg.BindAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)
}

// ---------- config ----------

type config struct {
	FleetcomURL   string   // base URL, e.g. https://fleet.barta.cm
	FleetcomToken string   // same bearer token as Bosun on this host
	LogContainer  string   // docker container to tail, default "openclaw-gateway"
	BindAddr      string   // where to serve /v1/agent-state
	AgentNames    []string // agents to track (merlin, nimue)
	AgentType     string   // "openclaw" by default
	HostName      string   // for the event.agent.host field
}

func loadConfig() config {
	c := config{
		FleetcomURL:   getenv("FLEETCOM_URL", "https://fleet.barta.cm"),
		FleetcomToken: os.Getenv("FLEETCOM_TOKEN"),
		LogContainer:  getenv("BRIDGE_LOG_CONTAINER", "openclaw-gateway"),
		BindAddr:      getenv("BRIDGE_BIND_ADDR", ":9180"),
		AgentNames:    splitCSV(getenv("BRIDGE_AGENT_NAMES", "merlin,nimue")),
		AgentType:     getenv("BRIDGE_AGENT_TYPE", "openclaw"),
		HostName:      getenv("FLEETCOM_HOSTNAME", hostname()),
	}
	if c.FleetcomToken == "" {
		log.Println("warning: FLEETCOM_TOKEN is empty — event push will be skipped")
	}
	return c
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// ---------- state ----------

type agentState struct {
	Host             string            `json:"host"`
	Name             string            `json:"name"`
	AgentType        string            `json:"agent_type"`
	Status           string            `json:"status"`
	StatusSince      string            `json:"status_since"`
	CurrentTurnID    string            `json:"current_turn_id,omitempty"`
	Typing           *typing           `json:"typing,omitempty"`
	LastReplyPerChat map[string]string `json:"last_reply_per_chat,omitempty"`
	LastError        *errorSummary     `json:"last_error,omitempty"`
	Config           *stateConfig      `json:"config,omitempty"`
}

type typing struct {
	Active    bool   `json:"active"`
	ChatID    string `json:"chat_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type errorSummary struct {
	Class       string `json:"class"`
	TS          string `json:"ts"`
	MessageHash string `json:"message_hash,omitempty"`
}

type stateConfig struct {
	StuckThresholdSec int  `json:"stuck_threshold_sec,omitempty"`
	StuckSilenceSec   int  `json:"stuck_silence_sec,omitempty"`
	EmitExcerpts      bool `json:"emit_excerpts,omitempty"`
}

type stateMgr struct {
	mu     sync.Mutex
	agents map[string]*agentState
	host   string
}

func newState(names []string, agentType string) *stateMgr {
	m := &stateMgr{
		agents: make(map[string]*agentState, len(names)),
		host:   getenv("FLEETCOM_HOSTNAME", hostname()),
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, n := range names {
		m.agents[n] = &agentState{
			Host:        m.host,
			Name:        n,
			AgentType:   agentType,
			Status:      "idle",
			StatusSince: now,
			Config: &stateConfig{
				StuckThresholdSec: 120,
				StuckSilenceSec:   120,
			},
		}
	}
	return m
}

func (s *stateMgr) snapshot() []agentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agentState, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, *a)
	}
	return out
}

// apply moves state forward for one agent based on an observed event.
func (s *stateMgr) apply(agent string, kind, turnID string, now string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agent]
	if !ok {
		a = &agentState{Host: s.host, Name: agent, AgentType: "openclaw"}
		s.agents[agent] = a
	}
	a.StatusSince = now
	switch kind {
	case "turn.started":
		a.Status = "thinking"
		a.CurrentTurnID = turnID
	case "turn.tool-invoked":
		a.Status = "tool-running"
	case "turn.tool-completed":
		// Tool ended — assume we're back in thinking/replying.
		if a.Status == "tool-running" {
			a.Status = "thinking"
		}
	case "turn.replied":
		a.Status = "idle"
		a.CurrentTurnID = ""
	case "turn.errored":
		a.Status = "error"
		a.LastError = &errorSummary{Class: "error", TS: now}
	case "turn.abandoned":
		a.Status = "idle"
		a.CurrentTurnID = ""
	}
}

// abandonInFlight produces turn.abandoned events for any agent mid-turn
// so the dashboard clears zombies on bridge restart.
func (s *stateMgr) abandonInFlight() []outEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []outEvent
	now := time.Now().UTC().Format(time.RFC3339)
	for _, a := range s.agents {
		if a.CurrentTurnID == "" {
			continue
		}
		out = append(out, outEvent{
			Agent:   ref{Host: a.Host, Name: a.Name},
			TS:      now,
			Kind:    "turn.abandoned",
			TurnID:  a.CurrentTurnID,
			Payload: json.RawMessage(`{"reason":"bridge-shutdown"}`),
		})
	}
	return out
}

// ---------- emitter ----------

type ref struct {
	Host string `json:"host"`
	Name string `json:"name"`
}

type outEvent struct {
	Agent   ref             `json:"agent"`
	TS      string          `json:"ts"`
	Kind    string          `json:"kind"`
	TurnID  string          `json:"turn_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type emitter struct {
	cfg    config
	client *http.Client
}

func newEmitter(c config) *emitter {
	return &emitter{cfg: c, client: &http.Client{Timeout: 3 * time.Second}}
}

func (e *emitter) send(ev outEvent) {
	if e.cfg.FleetcomToken == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"event": ev})
	if err != nil {
		log.Printf("marshal event: %v", err)
		return
	}
	url := strings.TrimRight(e.cfg.FleetcomURL, "/") + "/api/agent-events"
	// Fire-and-forget with 3 retries, small backoff.
	go func() {
		for attempt := 0; attempt < 3; attempt++ {
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+e.cfg.FleetcomToken)
			resp, err := e.client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode < 500 {
					return
				}
			}
			time.Sleep(time.Duration(1<<attempt) * 250 * time.Millisecond)
		}
		log.Printf("event dropped after 3 retries: %s %s", ev.Kind, ev.TurnID)
	}()
}

// ---------- log tailer (sole event source in MVP) ----------

// tailDockerLogs runs `docker logs -f <container>` and feeds each line
// through the log parser. Crash-restart loop: if docker logs exits or
// errors, wait 2s and retry indefinitely.
func tailDockerLogs(ctx context.Context, container string, state *stateMgr, emitter *emitter) {
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(ctx, "docker", "logs", "-f", "--tail", "0", container)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("docker logs stdout pipe: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			log.Printf("docker logs start: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		scanLogs(ctx, stdout, state, emitter)
		cmd.Wait()
		if ctx.Err() == nil {
			log.Printf("docker logs exited; reconnecting in 2s")
			time.Sleep(2 * time.Second)
		}
	}
}

// scanLogs reads lines from r, parses each against the pattern set, and
// forwards any matches to state + emitter.
func scanLogs(ctx context.Context, r io.Reader, state *stateMgr, emitter *emitter) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Text()
		ev, ok := parseLine(line)
		if !ok {
			continue
		}
		state.apply(ev.agent, ev.kind, ev.turnID, ev.ts)
		emitter.send(outEvent{
			Agent:   ref{Host: state.host, Name: ev.agent},
			TS:      ev.ts,
			Kind:    ev.kind,
			TurnID:  ev.turnID,
			Payload: ev.payload,
		})
	}
}
