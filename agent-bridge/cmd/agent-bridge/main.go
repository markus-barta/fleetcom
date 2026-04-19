// agent-bridge is the OpenClaw → FleetCom event relay. It runs
// alongside an OpenClaw gateway on each host, speaks the gateway's
// Ed25519-signed WebSocket protocol directly (no log-tailing, no
// docker socket needed), subscribes to `session.message` events, and
// forwards a flattened event stream to FleetCom's
// /api/agent-events endpoint.
//
// Trust model: on first boot the bridge generates an Ed25519 keypair,
// persists it to a Docker volume, and POSTs its fingerprint to
// FleetCom at /api/bridges/register using the same bearer token bosun
// uses. FleetCom's gateway-scoped keypair then auto-approves the
// pairing on the gateway side. See docs/AGENT-BRIDGE-PAIRING.md in the
// fleetcom repo for the full picture.
//
// Schema: see fleetcom/docs/AGENT-OBSERVABILITY.md for the agent-event
// shape we produce.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/markus-barta/fleetcom/agent-bridge/internal/openclaw"
)

var version = "0.3.0"

func main() {
	cfg := loadConfig()
	log.Printf("fleetcom agent-bridge %s starting: host=%s agents=%v fc=%s gw=%s keys=%s",
		version, cfg.HostName, cfg.AgentNames, cfg.FleetcomURL, cfg.GatewayURL, cfg.KeyDir)

	// Load or generate persistent Ed25519 identity.
	id, err := loadOrGenerateIdentity(cfg.KeyDir)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	log.Printf("identity ready: deviceId=%s", id.DeviceID[:12])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shut down on SIGTERM so in-flight work has a chance to flush.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("shutdown: cancelling context")
		cancel()
	}()

	fc := &fleetcomClient{url: cfg.FleetcomURL, token: cfg.FleetcomToken, http: &http.Client{Timeout: 5 * time.Second}}
	state := newStateMgr(cfg.HostName, cfg.AgentNames, cfg.AgentType)

	// Register with FleetCom (retry forever — the trust anchor for
	// auto-approval). Nothing else waits for this; it runs in the
	// background because the gateway will queue our pair request once
	// we connect regardless.
	if cfg.FleetcomToken == "" {
		log.Println("warning: FLEETCOM_TOKEN is empty — skipping /api/bridges/register")
	} else {
		go registerLoop(ctx, fc, cfg.HostName, cfg.AgentNames, id)
	}

	// HTTP server: bosun still scrapes /v1/agent-state.
	go runHTTPServer(ctx, cfg.BindAddr, state)

	if cfg.GatewayURL == "" {
		log.Println("warning: OPENCLAW_GATEWAY_URL is empty — skipping gateway connection (bridge will run in state-only mode)")
		<-ctx.Done()
		return
	}

	// Load any persisted operator token the gateway gave us in a prior
	// hello-ok — lets us skip first-time pairing on reconnect.
	operatorToken := readFile(filepath.Join(cfg.KeyDir, "operator-token"))

	trans := &translator{
		fc:    fc,
		state: state,
		host:  cfg.HostName,
	}

	var client *openclaw.Client
	client = openclaw.NewClient(openclaw.ClientOptions{
		URL:           cfg.GatewayURL,
		Identity:      id,
		OperatorToken: operatorToken,
		Role:          "operator",
		Scopes:        []string{"operator.read"},
		ClientID:      "gateway-client",
		ClientMode:    "backend",
		ClientVersion: "fleetcom-agent-bridge/" + version,
		Platform:      "linux",
		DeviceFamily:  "fleetcom-agent-bridge",
		OnEvent: func(event string, payload json.RawMessage) {
			trans.handleEvent(ctx, event, payload)
		},
		OnConnected: func(hello json.RawMessage) {
			log.Printf("gateway session live — hello=%s", truncate(string(hello), 200))
			// Persist any fresh device token the gateway handed us.
			persistHelloToken(cfg.KeyDir, hello)
			// Kick off a sessions.list + subscribe pass so we start
			// receiving session.message events immediately instead of
			// only on the next `sessions.changed` tick.
			go subscribeAllSessions(ctx, client, trans)
		},
	})

	if err := client.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("gateway client exited: %v", err)
	}
}

// ---------- config ----------

type config struct {
	FleetcomURL   string
	FleetcomToken string
	GatewayURL    string   // wss://127.0.0.1:18789 by convention
	HostName      string   // matches FleetCom's hosts.hostname
	AgentNames    []string // informational — what to report in /v1/agent-state
	AgentType     string
	BindAddr      string
	KeyDir        string // /var/lib/fleetcom-agent-bridge by convention
}

func loadConfig() config {
	c := config{
		FleetcomURL:   getenv("FLEETCOM_URL", "https://fleet.barta.cm"),
		FleetcomToken: os.Getenv("FLEETCOM_TOKEN"),
		GatewayURL:    getenv("OPENCLAW_GATEWAY_URL", "wss://127.0.0.1:18789"),
		HostName:      getenv("FLEETCOM_HOSTNAME", hostname()),
		AgentNames:    splitCSV(getenv("BRIDGE_AGENT_NAMES", "merlin,nimue")),
		AgentType:     getenv("BRIDGE_AGENT_TYPE", "openclaw"),
		BindAddr:      getenv("BRIDGE_BIND_ADDR", ":9180"),
		KeyDir:        getenv("BRIDGE_KEY_DIR", "/var/lib/fleetcom-agent-bridge"),
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

func hostname() string { h, _ := os.Hostname(); return h }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- identity ----------

func loadOrGenerateIdentity(dir string) (*openclaw.Identity, error) {
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")
	if _, err := os.Stat(privPath); err == nil {
		return openclaw.LoadIdentity(privPath, pubPath)
	}
	log.Printf("no keypair found; generating fresh Ed25519 identity at %s", dir)
	return openclaw.GenerateIdentity(privPath, pubPath)
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// persistHelloToken inspects the gateway's hello-ok payload for an
// `auth.deviceToken` field (the OpenClaw client stores this so next
// reconnect includes it). Persisting it means we skip pair.pending
// after the very first successful pairing.
func persistHelloToken(dir string, hello json.RawMessage) {
	var h struct {
		Auth struct {
			DeviceToken string `json:"deviceToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(hello, &h); err != nil || h.Auth.DeviceToken == "" {
		return
	}
	path := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(path, []byte(h.Auth.DeviceToken+"\n"), 0o600); err != nil {
		log.Printf("persist operator token: %v", err)
		return
	}
	log.Printf("persisted operator token to %s", path)
}

// ---------- FleetCom HTTP client ----------

type fleetcomClient struct {
	url   string
	token string
	http  *http.Client
}

func (c *fleetcomClient) post(path string, body interface{}, reply interface{}) error {
	if c.token == "" {
		return errors.New("no token")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(c.url, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s → %d: %s", path, resp.StatusCode, string(data))
	}
	if reply != nil {
		return json.NewDecoder(resp.Body).Decode(reply)
	}
	return nil
}

// registerLoop POSTs /api/bridges/register once for each agent we
// serve. FleetCom uses the (host, agent) pair to dedupe, so re-calling
// is idempotent. Retry forever — the operator might be starting the
// stack one service at a time.
func registerLoop(ctx context.Context, fc *fleetcomClient, host string, agents []string, id *openclaw.Identity) {
	pubPEM, _ := os.ReadFile(filepath.Join(getenv("BRIDGE_KEY_DIR", "/var/lib/fleetcom-agent-bridge"), "public.pem"))
	for _, agent := range agents {
		body := map[string]string{
			"agent":      agent,
			"pubkey_pem": string(pubPEM),
		}
		for {
			if ctx.Err() != nil {
				return
			}
			var reply map[string]any
			if err := fc.post("/api/bridges/register", body, &reply); err != nil {
				log.Printf("fleetcom register %s: %v — retrying in 30s", agent, err)
				select {
				case <-time.After(30 * time.Second):
				case <-ctx.Done():
					return
				}
				continue
			}
			log.Printf("fleetcom register %s: ok (fp=%v auto_approve=%v)", agent, reply["fingerprint"], reply["auto_approve"])
			break
		}
	}
}

// ---------- state (bosun-scraped) ----------

type agentState struct {
	Host          string `json:"host"`
	Name          string `json:"name"`
	AgentType     string `json:"agent_type"`
	Status        string `json:"status"`
	StatusSince   string `json:"status_since"`
	CurrentTurnID string `json:"current_turn_id,omitempty"`
}

type stateMgr struct {
	mu     sync.Mutex
	agents map[string]*agentState
}

func newStateMgr(host string, names []string, agentType string) *stateMgr {
	m := &stateMgr{agents: make(map[string]*agentState, len(names))}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, n := range names {
		m.agents[n] = &agentState{
			Host: host, Name: n, AgentType: agentType,
			Status: "idle", StatusSince: now,
		}
	}
	return m
}

func (m *stateMgr) snapshot() []agentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]agentState, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, *a)
	}
	return out
}

// ---------- HTTP server (bosun probe) ----------

func runHTTPServer(ctx context.Context, addr string, state *stateMgr) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent-state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": 1,
			"agents":         state.snapshot(),
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shCtx)
	}()
	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}

// ---------- translator (OpenClaw events → FleetCom agent-events) ----------

type translator struct {
	fc    *fleetcomClient
	state *stateMgr
	host  string
}

// handleEvent dispatches gateway events. We care about:
//   - session.message   — user↔agent message lifecycle (the meat)
//   - sessions.changed  — re-subscribe signal (new session appeared)
//
// Everything else is logged sparsely so you can see the stream without
// flooding. Translation to FleetCom's agent-event schema is intentionally
// conservative in this first pass — we forward each OpenClaw event as
// an opaque payload and let the dashboard decide how to render. A
// richer mapping follows once we see real-world shapes on hsb0.
func (t *translator) handleEvent(ctx context.Context, event string, payload json.RawMessage) {
	switch event {
	case "session.message":
		t.forwardMessage(ctx, payload)
	case "sessions.changed":
		// The outer loop re-runs subscribeAllSessions on each
		// sessions.changed tick, so ignore here.
	case "tick", "connect.challenge":
		// Noise; skip.
	default:
		if len(payload) > 0 {
			log.Printf("event %s: %s", event, truncate(string(payload), 160))
		} else {
			log.Printf("event %s", event)
		}
	}
}

// forwardMessage extracts the fields we know about (agentId, sessionKey,
// role, timestamp) and POSTs a minimally-shaped agent-event so the
// dashboard sees activity. The full OpenClaw message is attached as the
// payload so the translator can be enriched later without a bridge
// redeploy.
func (t *translator) forwardMessage(ctx context.Context, payload json.RawMessage) {
	var p struct {
		SessionKey string          `json:"sessionKey"`
		Message    json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("session.message unmarshal: %v", err)
		return
	}
	var msg struct {
		Role      string `json:"role"`
		AgentID   string `json:"agentId"`
		Timestamp int64  `json:"timestamp"`
	}
	_ = json.Unmarshal(p.Message, &msg)
	agent := msg.AgentID
	if agent == "" {
		agent = "unknown"
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if msg.Timestamp > 0 {
		ts = time.UnixMilli(msg.Timestamp).UTC().Format(time.RFC3339)
	}

	kind := "turn.tool-invoked"
	if msg.Role == "assistant" {
		kind = "turn.replied"
	} else if msg.Role == "user" {
		kind = "turn.started"
	}

	ev := map[string]any{
		"event": map[string]any{
			"agent":   map[string]any{"host": t.host, "name": agent},
			"ts":      ts,
			"kind":    kind,
			"turn_id": p.SessionKey,
			"payload": p.Message,
		},
	}
	if err := t.fc.post("/api/agent-events", ev, nil); err != nil {
		log.Printf("fleetcom agent-events: %v", err)
	}
}

// subscribeAllSessions fetches the gateway's session list and calls
// sessions.messages.subscribe for each. The gateway dedupes subscribe
// calls per (connId, key), so it's safe to run on every reconnect
// AND on every sessions.changed event.
func subscribeAllSessions(ctx context.Context, client *openclaw.Client, t *translator) {
	raw, err := client.Call(ctx, "sessions.list", map[string]any{}, 10*time.Second)
	if err != nil {
		log.Printf("sessions.list: %v", err)
		return
	}
	var r struct {
		Sessions []struct {
			Key string `json:"key"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		log.Printf("sessions.list unmarshal: %v", err)
		return
	}
	for _, s := range r.Sessions {
		if s.Key == "" {
			continue
		}
		if _, err := client.Call(ctx, "sessions.messages.subscribe", map[string]any{"key": s.Key}, 5*time.Second); err != nil {
			log.Printf("subscribe %s: %v", s.Key, err)
			continue
		}
	}
	log.Printf("subscribed to %d sessions", len(r.Sessions))
}
