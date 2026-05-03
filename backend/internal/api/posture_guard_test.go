package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// QA-AUDIT-FIX (PPM 1527) — regression tests for the symmetric guard
// that refuses non-canonical (auto-approve ON + OOB ON) combinations
// at the toggle layer. With both ON, RegisterBridge would silently take
// the auto-approve path and never enforce OOB; better to reject here.

func newPostureGuardServer(t *testing.T) (*db.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "fleetcom-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	hub := sse.NewHub()
	r := chi.NewRouter()
	r.Post("/api/gateways/{host}/auto-approve/{mode}", SetGatewayAutoApprove(store, hub))
	r.Post("/api/gateways/{host}/oob-delivery/{mode}", SetGatewayOOBDelivery(store, hub))
	if _, err := store.UpsertGateway("dsc0", "wss://dsc0:18789"); err != nil {
		t.Fatalf("UpsertGateway: %v", err)
	}
	return store, r
}

func postSwitch(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestPostureGuard_OOBOnWhileAutoOn_Rejected(t *testing.T) {
	store, h := newPostureGuardServer(t)
	if err := store.SetAutoApprove("dsc0", true); err != nil {
		t.Fatalf("SetAutoApprove: %v", err)
	}

	rr := postSwitch(t, h, "/api/gateways/dsc0/oob-delivery/on")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (auto+OOB combo rejected), got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "auto-approve OFF") {
		t.Fatalf("error body should hint at auto-approve OFF; got %q", rr.Body.String())
	}
	// Confirm OOB stayed OFF — guard is supposed to reject before mutating.
	gws, _ := store.AllGateways()
	for _, g := range gws {
		if g.Host == "dsc0" && g.OOBDeliveryEnabled {
			t.Fatalf("OOB was enabled despite 422 — guard didn't gate the mutation")
		}
	}
}

func TestPostureGuard_AutoOnWhileOOBOn_Rejected(t *testing.T) {
	store, h := newPostureGuardServer(t)
	if err := store.SetOOBDelivery("dsc0", true); err != nil {
		t.Fatalf("SetOOBDelivery (seed): %v", err)
	}

	rr := postSwitch(t, h, "/api/gateways/dsc0/auto-approve/on")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (OOB+auto combo rejected), got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "OOB") {
		t.Fatalf("error body should mention OOB; got %q", rr.Body.String())
	}
	gws, _ := store.AllGateways()
	for _, g := range gws {
		if g.Host == "dsc0" && g.AutoApproveBridges {
			t.Fatalf("auto was enabled despite 422")
		}
	}
}

// Both directions must allow OFF — turning a flag off is always safe.
func TestPostureGuard_TurnOff_AlwaysAllowed(t *testing.T) {
	store, h := newPostureGuardServer(t)
	// Hardened-ish state — both OFF, attestation ON. Turning OOB ON
	// here is allowed; that's a valid Reviewed→Hardened transition.
	rr := postSwitch(t, h, "/api/gateways/dsc0/oob-delivery/on")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("OOB ON with auto OFF should succeed, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Now turn OOB OFF — should always work.
	rr = postSwitch(t, h, "/api/gateways/dsc0/oob-delivery/off")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("OOB OFF should always succeed, got %d", rr.Code)
	}
	// And same for auto-approve OFF.
	rr = postSwitch(t, h, "/api/gateways/dsc0/auto-approve/off")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("auto OFF should always succeed, got %d", rr.Code)
	}
	_ = store
}
