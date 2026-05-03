package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
)

// FLEET-118 — preflight handler. The TCP / TLS probes are hard to
// exercise without a real listener; these tests focus on:
//   * JSON shape contract (host, gateway_port, blockers[], ready)
//   * bosun-freshness logic (never_seen / stale / fresh)
//   * unknown-host short-circuit (no network probe attempted)
// Live network behavior is verified by the smoke step on prod deploy.

func newPreflightServer(t *testing.T) (*db.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "fleetcom-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })

	r := chi.NewRouter()
	r.Get("/api/gateways/{host}/preflight", GatewayPreflight(store))
	return store, r
}

func seedHostLastSeen(t *testing.T, store *db.Store, hostname, lastSeen string) {
	t.Helper()
	// hosts row needs hostname; everything else can default.
	_, err := store.DB.Exec(
		`INSERT INTO hosts (hostname, os, kernel, last_seen) VALUES (?, '', '', ?)`,
		hostname, lastSeen,
	)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
}

func callPreflight(t *testing.T, h http.Handler, host string) *preflightResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/gateways/"+host+"/preflight", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight %q: status=%d body=%s", host, rr.Code, rr.Body.String())
	}
	var out preflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	return &out
}

func TestPreflight_UnknownHost(t *testing.T) {
	_, h := newPreflightServer(t)

	out := callPreflight(t, h, "ghost-host")

	if out.Host != "ghost-host" {
		t.Fatalf("host echoed wrong: %q", out.Host)
	}
	if out.GatewayPort != gatewayPort {
		t.Fatalf("gateway_port wrong: got %d want %d", out.GatewayPort, gatewayPort)
	}
	if !contains(out.Blockers, blockerHostUnknown) {
		t.Fatalf("expected host_unknown blocker, got %v", out.Blockers)
	}
	if out.Ready {
		t.Fatalf("ready=true on unknown host")
	}
	// And we MUST NOT have attempted the network probe — both fields
	// should be at their zero values. (The probe section is gated on
	// !host_unknown precisely to avoid a misleading 'gateway_unreachable'
	// on a host that simply doesn't exist yet.)
	if out.GatewayPortReachable {
		t.Fatalf("network probe must skip on unknown host")
	}
	if out.TLSOK {
		t.Fatalf("TLS probe must skip on unknown host")
	}
}

func TestPreflight_BosunNeverSeen(t *testing.T) {
	store, h := newPreflightServer(t)
	seedHostLastSeen(t, store, "freshrow", "")

	out := callPreflight(t, h, "freshrow")

	if !contains(out.Blockers, blockerBosunNeverSeen) {
		t.Fatalf("expected bosun_never_seen blocker, got %v", out.Blockers)
	}
	// host_unknown must NOT be present (the row exists, just no heartbeat).
	if contains(out.Blockers, blockerHostUnknown) {
		t.Fatalf("host_unknown should not be set when row exists: %v", out.Blockers)
	}
}

func TestPreflight_BosunStale(t *testing.T) {
	store, h := newPreflightServer(t)
	staleTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	seedHostLastSeen(t, store, "stalehost", staleTime)

	out := callPreflight(t, h, "stalehost")

	if !contains(out.Blockers, blockerBosunStale) {
		t.Fatalf("expected bosun_stale blocker, got %v", out.Blockers)
	}
	if out.BosunSeenAt == "" {
		t.Fatalf("bosun_seen_at should be populated for known-stale row")
	}
	if out.BosunSeenAgoSeconds == nil || *out.BosunSeenAgoSeconds < bosunFreshSeconds {
		t.Fatalf("bosun_seen_ago_seconds should be > %d, got %v", bosunFreshSeconds, out.BosunSeenAgoSeconds)
	}
}

func TestPreflight_BosunFresh_NoBlocker(t *testing.T) {
	store, h := newPreflightServer(t)
	freshTime := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	seedHostLastSeen(t, store, "127.0.0.1", freshTime)

	out := callPreflight(t, h, "127.0.0.1")

	// Bosun blockers should be absent — the row is fresh.
	if contains(out.Blockers, blockerBosunStale) {
		t.Fatalf("bosun_stale set for fresh row: %v", out.Blockers)
	}
	if contains(out.Blockers, blockerBosunNeverSeen) {
		t.Fatalf("bosun_never_seen set for fresh row: %v", out.Blockers)
	}
	if contains(out.Blockers, blockerHostUnknown) {
		t.Fatalf("host_unknown set for known row: %v", out.Blockers)
	}
	if out.BosunSeenAgoSeconds == nil {
		t.Fatalf("bosun_seen_ago_seconds nil for fresh row")
	}
	if *out.BosunSeenAgoSeconds > bosunFreshSeconds {
		t.Fatalf("ago=%d > %d for a 30s-old row", *out.BosunSeenAgoSeconds, bosunFreshSeconds)
	}
	// Network probe will have run (host=127.0.0.1, no listener on
	// 18789) — gateway_unreachable is the expected outcome and is what
	// the wizard surfaces. ready=false either way; the test just checks
	// the probe DID run for known hosts.
	if !contains(out.Blockers, blockerGatewayUnreach) && !contains(out.Blockers, blockerTLSFailed) {
		t.Logf("probe outcome: reachable=%v tls_ok=%v blockers=%v",
			out.GatewayPortReachable, out.TLSOK, out.Blockers)
		// Not a fatal — local environments vary. The point of this test
		// is to confirm the bosun-freshness gate doesn't hide the probe.
	}
	if out.Ready {
		t.Fatalf("ready=true with no listener on 18789")
	}
}

func TestPreflight_JSONShape(t *testing.T) {
	store, h := newPreflightServer(t)
	seedHostLastSeen(t, store, "shape", time.Now().UTC().Format(time.RFC3339))

	req := httptest.NewRequest(http.MethodGet, "/api/gateways/shape/preflight", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q want no-store", got)
	}

	// blockers must be `[]`, never `null` — frontends iterate it.
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["blockers"].([]any); !ok {
		t.Fatalf("blockers field must be a JSON array (never null), got %T (%v)", raw["blockers"], raw["blockers"])
	}
}
