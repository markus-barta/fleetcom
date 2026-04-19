package openclaw

import (
	"context"
	"encoding/json"
	"errors"
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

// NewManager builds a manager. keyDir is where per-gateway keypairs and
// operator tokens are expected to live (production: `/run/agenix`; dev:
// whatever the operator configures). version is included in the
// handshake client metadata.
func NewManager(store *db.Store, hub *sse.Hub, keyDir, version string) *Manager {
	return &Manager{
		store:   store,
		hub:     hub,
		keyDir:  keyDir,
		version: version,
		clients: make(map[string]*clientHandle),
	}
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

// reconcile diffs DB-paired-gateways against running clients and starts
// / stops as needed. Gateways without on-disk keypairs are skipped with
// a single log line so boot isn't noisy when FLEET-52 hasn't deployed
// secrets yet.
func (m *Manager) reconcile(ctx context.Context) {
	gws, err := m.store.AllGateways()
	if err != nil {
		log.Printf("openclaw reconcile: %v", err)
		return
	}

	want := make(map[string]db.OpenClawGateway)
	for _, g := range gws {
		if g.Status == "paired" && g.URL != "" {
			want[g.Host] = g
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for host, h := range m.clients {
		if _, ok := want[host]; !ok {
			log.Printf("openclaw: stopping client for %s (no longer paired)", host)
			h.cancel()
			delete(m.clients, host)
		}
	}

	for host, g := range want {
		if _, ok := m.clients[host]; ok {
			continue
		}
		id, token, err := m.loadGatewayCreds(host)
		if err != nil {
			// Quiet if files simply don't exist yet; noisy if malformed.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			log.Printf("openclaw: load creds for %s: %v", host, err)
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		host := host // capture
		cl := NewClient(ClientOptions{
			URL:           g.URL,
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
		log.Printf("openclaw: started client for %s (%s)", host, g.URL)
	}
}

// loadGatewayCreds reads the Ed25519 private key and operator token for
// one gateway from keyDir. Conventional filenames:
//
//	fleetcom-openclaw-<host>-key     (PKCS#8 Ed25519 PEM)
//	fleetcom-openclaw-<host>-tok     (operator token, one line)
func (m *Manager) loadGatewayCreds(host string) (*Identity, string, error) {
	privPath := filepath.Join(m.keyDir, "fleetcom-openclaw-"+host+"-key")
	tokPath := filepath.Join(m.keyDir, "fleetcom-openclaw-"+host+"-tok")
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
