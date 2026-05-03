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

// FLEET-121 — onboarding state. Covers the three actionable buckets,
// the wizard_actionable rollup, and stable JSON shape (always-array
// fields, never null).

func newOnboardingServer(t *testing.T) (*db.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "fleetcom-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	r := chi.NewRouter()
	r.Get("/api/onboarding/state", OnboardingState(store))
	return store, r
}

func seedOnboardingHost(t *testing.T, store *db.Store, hostname string, lastSeen string) {
	t.Helper()
	_, err := store.DB.Exec(
		`INSERT INTO hosts (hostname, os, kernel, last_seen) VALUES (?, '', '', ?)`,
		hostname, lastSeen,
	)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
}

func seedOnboardingGateway(t *testing.T, store *db.Store, host, status string) {
	t.Helper()
	if _, err := store.UpsertGateway(host, "wss://"+host+":18789"); err != nil {
		t.Fatalf("UpsertGateway: %v", err)
	}
	if status != "" && status != "unpaired" {
		// MarkGatewayPaired sets status='paired'.
		if err := store.MarkGatewayPaired(host, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ""); err != nil {
			t.Fatalf("MarkGatewayPaired: %v", err)
		}
	}
}

func seedOnboardingBridge(t *testing.T, store *db.Store, host, agent, status string) {
	t.Helper()
	if err := store.RegisterBridge(host, agent, "abcd"+agent, "PEM"); err != nil {
		t.Fatalf("RegisterBridge: %v", err)
	}
	if status == "approved" {
		if err := store.MarkBridgeApprovedManual(host, agent); err != nil {
			t.Fatalf("MarkBridgeApprovedManual: %v", err)
		}
	}
	// Default state from RegisterBridge is 'pending' — leave it for
	// the pending-approval bucket.
}

func callOnboarding(t *testing.T, h http.Handler) *onboardingState {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/onboarding/state", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out onboardingState
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	return &out
}

func TestOnboarding_EmptyFleet_NotActionable(t *testing.T) {
	_, h := newOnboardingServer(t)
	out := callOnboarding(t, h)
	if out.WizardActionable {
		t.Fatalf("empty fleet must not be actionable")
	}
	if len(out.HostsWithBosunNoGateway) != 0 ||
		len(out.HostsWithGatewayNoBridge) != 0 ||
		len(out.GatewaysPendingApproval) != 0 {
		t.Fatalf("expected all-empty buckets, got %+v", out)
	}
}

func TestOnboarding_HostNeverSeen_Skipped(t *testing.T) {
	store, h := newOnboardingServer(t)
	// Host registered but bosun has never reported in (last_seen empty).
	// The wizard treats this as "go bring up bosun first" — outside scope.
	seedOnboardingHost(t, store, "ghost", "")

	out := callOnboarding(t, h)
	if out.WizardActionable {
		t.Fatalf("never-seen-bosun host should not be actionable: %+v", out)
	}
}

func TestOnboarding_HostNeedsGateway(t *testing.T) {
	store, h := newOnboardingServer(t)
	seedOnboardingHost(t, store, "dsc0", time.Now().UTC().Format(time.RFC3339))

	out := callOnboarding(t, h)
	if !out.WizardActionable {
		t.Fatalf("host with bosun and no gateway should be actionable")
	}
	if len(out.HostsWithBosunNoGateway) != 1 ||
		out.HostsWithBosunNoGateway[0].Hostname != "dsc0" {
		t.Fatalf("expected dsc0 in bosun-no-gateway: %+v", out.HostsWithBosunNoGateway)
	}
}

func TestOnboarding_UnpairedGatewayCountsAsNoGateway(t *testing.T) {
	store, h := newOnboardingServer(t)
	seedOnboardingHost(t, store, "dsc0", time.Now().UTC().Format(time.RFC3339))
	// Gateway row exists but status is unpaired (default after UpsertGateway
	// without MarkGatewayPaired). Should still surface as needing pairing.
	seedOnboardingGateway(t, store, "dsc0", "unpaired")

	out := callOnboarding(t, h)
	if len(out.HostsWithBosunNoGateway) != 1 {
		t.Fatalf("unpaired gateway should count as no-gateway: %+v", out)
	}
}

func TestOnboarding_PairedGatewayNeedsBridge(t *testing.T) {
	store, h := newOnboardingServer(t)
	seedOnboardingHost(t, store, "dsc0", time.Now().UTC().Format(time.RFC3339))
	seedOnboardingGateway(t, store, "dsc0", "paired")

	out := callOnboarding(t, h)
	if len(out.HostsWithBosunNoGateway) != 0 {
		t.Fatalf("paired gateway should not appear in bosun-no-gateway: %+v", out)
	}
	if len(out.HostsWithGatewayNoBridge) != 1 ||
		out.HostsWithGatewayNoBridge[0].Hostname != "dsc0" {
		t.Fatalf("expected dsc0 in gateway-no-bridge: %+v", out)
	}
	if !out.WizardActionable {
		t.Fatalf("expected actionable")
	}
}

func TestOnboarding_PendingBridgeAndApprovedBridge(t *testing.T) {
	store, h := newOnboardingServer(t)
	seedOnboardingHost(t, store, "dsc0", time.Now().UTC().Format(time.RFC3339))
	seedOnboardingGateway(t, store, "dsc0", "paired")
	seedOnboardingBridge(t, store, "dsc0", "merlin", "approved")
	seedOnboardingBridge(t, store, "dsc0", "ocean", "pending")

	out := callOnboarding(t, h)
	// dsc0 has an approved bridge → should NOT be in gateway-no-bridge.
	if len(out.HostsWithGatewayNoBridge) != 0 {
		t.Fatalf("host with approved bridge should not be in gateway-no-bridge: %+v", out)
	}
	// ocean is pending → should be in pending-approval.
	if len(out.GatewaysPendingApproval) != 1 ||
		out.GatewaysPendingApproval[0].Agent != "ocean" ||
		out.GatewaysPendingApproval[0].Host != "dsc0" {
		t.Fatalf("expected ocean@dsc0 in pending-approval: %+v", out)
	}
	if !out.WizardActionable {
		t.Fatalf("expected actionable (pending row counts)")
	}
}

func TestOnboarding_FullSetup_NotActionable(t *testing.T) {
	store, h := newOnboardingServer(t)
	seedOnboardingHost(t, store, "dsc0", time.Now().UTC().Format(time.RFC3339))
	seedOnboardingGateway(t, store, "dsc0", "paired")
	seedOnboardingBridge(t, store, "dsc0", "merlin", "approved")

	out := callOnboarding(t, h)
	if out.WizardActionable {
		t.Fatalf("fully-set-up fleet should not be actionable: %+v", out)
	}
}

func TestOnboarding_JSONShape(t *testing.T) {
	_, h := newOnboardingServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/onboarding/state", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q want no-store", got)
	}

	// All three array fields must be `[]` never `null` — frontends iterate.
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"hosts_with_bosun_no_gateway", "hosts_with_gateway_no_bridge", "gateways_pending_approval"} {
		if _, ok := raw[key].([]any); !ok {
			t.Fatalf("%s must be JSON array, got %T (%v)", key, raw[key], raw[key])
		}
	}
}

func TestOnboarding_StableSortOrder(t *testing.T) {
	store, h := newOnboardingServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	// Insert hosts out of alphabetical order.
	seedOnboardingHost(t, store, "zeta", now)
	seedOnboardingHost(t, store, "alpha", now)
	seedOnboardingHost(t, store, "midi", now)

	out := callOnboarding(t, h)
	if len(out.HostsWithBosunNoGateway) != 3 {
		t.Fatalf("expected 3 hosts in bucket: %+v", out)
	}
	want := []string{"alpha", "midi", "zeta"}
	for i, w := range want {
		if out.HostsWithBosunNoGateway[i].Hostname != w {
			t.Fatalf("sort order wrong at [%d]: got %q want %q (full: %+v)",
				i, out.HostsWithBosunNoGateway[i].Hostname, w, out.HostsWithBosunNoGateway)
		}
	}
}
