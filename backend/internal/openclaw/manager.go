package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// Manager owns one WebSocket client per paired gateway. It reconciles
// the DB state (openclaw_gateways rows with status=paired) against the
// set of running client goroutines every 2 minutes, so admin actions
// that pair or revoke a gateway are picked up without a server restart.
//
// Each client's OnEvent callback routes `device.pair.requested` events
// into the auto-approver, which matches the incoming fingerprint
// against `bridge_pairings` and calls `device.pair.approve(requestId)`
// when a registration exists.
type Manager struct {
	store   *db.Store
	hub     *sse.Hub
	keyDir  string
	version string

	mu      sync.Mutex
	clients map[string]*clientHandle
}

type clientHandle struct {
	client *Client
	cancel context.CancelFunc
}

// NewManager builds a manager. keyDir is a colon-separated list of
// directories where per-gateway keypairs + operator tokens live. Both
// directories are scanned and merged (app-data takes precedence on
// name conflict, though that shouldn't happen in practice). Typical
// value: `/run/agenix:/app/data/openclaw-keys` — agenix for
// nixcfg-managed pairings, /app/data for the in-UI wizard (FLEET-61).
// version is included in the handshake client metadata.
func NewManager(store *db.Store, hub *sse.Hub, keyDir, version string) *Manager {
	return &Manager{
		store:   store,
		hub:     hub,
		keyDir:  keyDir,
		version: version,
		clients: make(map[string]*clientHandle),
	}
}

// keyDirs returns the list of directories to scan, split from the
// colon-separated keyDir string. Blank entries are dropped.
func (m *Manager) keyDirs() []string {
	var out []string
	for _, d := range strings.Split(m.keyDir, ":") {
		d = strings.TrimSpace(d)
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// Start kicks off the reconcile loop. Caller owns the context — cancel
// it to stop all clients.
func (m *Manager) Start(ctx context.Context) {
	m.reconcile(ctx)
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.reconcile(ctx)
			case <-ctx.Done():
				m.stopAll()
				return
			}
		}
	}()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, h := range m.clients {
		h.cancel()
		delete(m.clients, host)
	}
}

// reconcile discovers gateways from on-disk keypair files and ensures
// one running client per configured gateway. Auto-pairing semantics:
// the presence of `fleetcom-openclaw-<host>-key` in keyDir IS the
// operator's declaration that FleetCom should manage that gateway.
// (FLEET-52 agenix populates these; no separate "pair gateway" API is
// needed.) Rows in openclaw_gateways are kept in sync so the dashboard
// reflects reality.
func (m *Manager) reconcile(ctx context.Context) {
	keyHosts := m.scanKeyDir()

	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[string]bool, len(keyHosts))
	for _, h := range keyHosts {
		want[h] = true
	}

	for host, h := range m.clients {
		if !want[host] {
			log.Printf("openclaw: stopping client for %s (keypair removed)", host)
			h.cancel()
			delete(m.clients, host)
		}
	}

	for _, host := range keyHosts {
		if _, ok := m.clients[host]; ok {
			continue
		}
		id, token, err := m.loadGatewayCreds(host)
		if err != nil {
			log.Printf("openclaw %s: load creds: %v", host, err)
			continue
		}
		url := fmt.Sprintf("wss://%s:18789", host)
		if _, err := m.store.UpsertGateway(host, url); err != nil {
			log.Printf("openclaw %s: upsert gateway: %v", host, err)
		}
		// Mark paired unconditionally so the dashboard reflects that
		// FleetCom is configured for this gateway. Runtime connection
		// health surfaces via logs + (future) a per-client connected
		// indicator in the Gateways tab.
		if err := m.store.MarkGatewayPaired(host, id.PubKeyRawB64U, ""); err != nil {
			log.Printf("openclaw %s: mark paired: %v", host, err)
		}

		cctx, cancel := context.WithCancel(ctx)
		host := host // capture
		cl := NewClient(ClientOptions{
			URL:           url,
			Identity:      id,
			OperatorToken: token,
			Role:          "operator",
			Scopes:        []string{"operator.read", "operator.pairing"},
			ClientID:      "gateway-client",
			ClientMode:    "backend",
			ClientVersion: "fleetcom/" + m.version,
			Platform:      "linux",
			DeviceFamily:  "fleetcom-server",
			OnEvent: func(event string, payload json.RawMessage) {
				m.handleEvent(cctx, host, event, payload)
			},
			OnConnected: func(_ json.RawMessage) {
				log.Printf("openclaw %s: operator session live", host)
			},
		})
		m.clients[host] = &clientHandle{client: cl, cancel: cancel}
		go func() {
			if err := cl.Run(cctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("openclaw %s: client exited: %v", host, err)
			}
		}()
		log.Printf("openclaw: started client for %s (%s)", host, url)
	}

	// Broadcast gateway list update so the dashboard reflects any
	// newly-discovered rows immediately.
	if gs, err := m.store.AllGateways(); err == nil {
		if data, err := json.Marshal(gs); err == nil {
			m.hub.Broadcast("gateways", data)
		}
	}
}

// scanKeyDir walks every configured keyDir and returns a deduped set of
// hostnames that have usable on-disk credentials. Recognises both the
// flat (agenix) layout and the nested (app-data wizard) layout; see
// loadGatewayCreds for shapes.
func (m *Manager) scanKeyDir() []string {
	seen := map[string]struct{}{}
	for _, dir := range m.keyDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			// Layout 1 (flat): fleetcom-openclaw-<host>-key
			if !e.IsDir() && strings.HasPrefix(name, "fleetcom-openclaw-") && strings.HasSuffix(name, "-key") {
				host := strings.TrimSuffix(strings.TrimPrefix(name, "fleetcom-openclaw-"), "-key")
				if host != "" {
					seen[host] = struct{}{}
				}
				continue
			}
			// Layout 2 (nested): <host>/private.pem inside a per-host dir.
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(dir, name, "private.pem")); err == nil {
					seen[name] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	return out
}

// loadGatewayCreds walks the configured keyDirs and returns the first
// match for a given host. Supports two on-disk layouts:
//
//  1. Flat (agenix): <dir>/fleetcom-openclaw-<host>-{key,tok}
//     Typical for /run/agenix, where agenix flattens secret filenames.
//
//  2. Nested (app-data wizard): <dir>/<host>/{private.pem, operator-token}
//     Typical for /app/data/openclaw-keys, written by the POST /pair
//     endpoint in FLEET-61 where the fleetcom container can own its
//     own keypair directory.
func (m *Manager) loadGatewayCreds(host string) (*Identity, string, error) {
	for _, dir := range m.keyDirs() {
		// Try layout 1 (flat).
		privPath := filepath.Join(dir, "fleetcom-openclaw-"+host+"-key")
		tokPath := filepath.Join(dir, "fleetcom-openclaw-"+host+"-tok")
		if id, tok, err := tryLoadCreds(privPath, tokPath); err == nil {
			return id, tok, nil
		}
		// Try layout 2 (nested under host dir).
		privPath = filepath.Join(dir, host, "private.pem")
		tokPath = filepath.Join(dir, host, "operator-token")
		if id, tok, err := tryLoadCreds(privPath, tokPath); err == nil {
			return id, tok, nil
		}
	}
	return nil, "", os.ErrNotExist
}

func tryLoadCreds(privPath, tokPath string) (*Identity, string, error) {
	id, err := LoadIdentity(privPath, "")
	if err != nil {
		return nil, "", err
	}
	tokBytes, err := os.ReadFile(tokPath)
	if err != nil {
		return nil, "", err
	}
	return id, strings.TrimSpace(string(tokBytes)), nil
}

// handleEvent routes gateway-pushed events. Only pairing-related ones
// matter to us right now; everything else is logged at debug level.
func (m *Manager) handleEvent(ctx context.Context, host, event string, payload json.RawMessage) {
	switch event {
	case "device.pair.requested":
		m.handlePairRequested(ctx, host, payload)
	case "device.pair.resolved":
		// Reflect gateway-side changes into our local bridge_pairings
		// state. Currently informational only — when FLEET-50 lands we
		// may want to bump last_seen_at here.
		log.Printf("openclaw %s: pair resolved: %s", host, truncate(string(payload), 200))
	default:
		// ignore (tick, presence, etc.)
	}
}

// handlePairRequested is the auto-approver. OpenClaw emits this event
// whenever a bridge initiates pairing. We match the incoming deviceId
// against the host's registered bridges — if a bridge has already POSTed
// its pubkey to `/api/bridges/register` with a valid bosun token, we
// approve; otherwise the request sits pending for admin override.
func (m *Manager) handlePairRequested(ctx context.Context, host string, payload json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		DeviceID  string `json:"deviceId"`
		ClientID  string `json:"clientId"`
		Platform  string `json:"platform"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("openclaw %s: malformed pair.requested: %v", host, err)
		return
	}
	if p.RequestID == "" || p.DeviceID == "" {
		log.Printf("openclaw %s: pair.requested missing requestId/deviceId", host)
		return
	}

	// Gateway is not auto-approving? Leave pending — operator will click.
	gws, err := m.store.AllGateways()
	if err == nil {
		for _, g := range gws {
			if g.Host == host && !g.AutoApproveBridges {
				log.Printf("openclaw %s: pair.requested %s (deviceId=%s) left pending — auto-approve is off", host, p.RequestID, p.DeviceID[:12])
				return
			}
		}
	}

	// Fingerprint match: we store full sha256 hex in bridge_pairings.pubkey_fp.
	bridge, err := m.store.BridgeByFingerprint(host, p.DeviceID)
	if err != nil {
		log.Printf("openclaw %s: bridge lookup failed: %v", host, err)
		return
	}
	if bridge == nil {
		log.Printf("openclaw %s: pair.requested %s (deviceId=%s) — no matching bridge registration, left pending", host, p.RequestID, p.DeviceID[:12])
		return
	}

	m.mu.Lock()
	h, ok := m.clients[host]
	m.mu.Unlock()
	if !ok {
		log.Printf("openclaw %s: no client to approve with", host)
		return
	}

	if _, err := h.client.Call(ctx, "device.pair.approve", map[string]interface{}{
		"requestId": p.RequestID,
	}, 15*time.Second); err != nil {
		log.Printf("openclaw %s: approve %s failed: %v", host, p.RequestID, err)
		return
	}

	if err := m.store.MarkBridgeApproved(host, bridge.Agent, p.RequestID); err != nil {
		log.Printf("openclaw %s: mark bridge approved: %v", host, err)
	}
	log.Printf("openclaw %s: auto-approved bridge %s/%s (deviceId=%s)", host, host, bridge.Agent, p.DeviceID[:12])

	// Broadcast updated bridge list so the dashboard flips pending→approved live.
	if bs, err := m.store.AllBridgePairings(); err == nil {
		if data, err := json.Marshal(bs); err == nil {
			m.hub.Broadcast("bridges", data)
		}
	}
}

// RevokeBridgeOnGateway is called from DELETE /api/bridges/{host}/{agent}
// after the DB row is dropped. It asks the gateway to revoke the
// operator token for that deviceId so a revoked bridge can't reconnect
// until a fresh pairing. Idempotent — if the gateway client isn't
// connected, the revoke is a no-op (the DB row drop already prevents
// future auto-approvals).
func (m *Manager) RevokeBridgeOnGateway(ctx context.Context, host, deviceID string) error {
	m.mu.Lock()
	h, ok := m.clients[host]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	_, err := h.client.Call(ctx, "device.token.revoke", map[string]interface{}{
		"deviceId": deviceID,
	}, 10*time.Second)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
