package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// FLEET-149 — server-side agent-name isolation on /api/bridges/register.
// The bosun side (FLEET-149) already guards bridge.install, but a
// compromised or out-of-band bridge could still try to register under
// any name. The server is the last line of defense — verify the
// requested agent matches one the host has heartbeat-reported.

func newBridgeRegisterEnv(t *testing.T) (*db.Store, http.Handler, string /*hostname*/, string /*plain bearer*/) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "fleetcom-bridge-iso.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })

	hostname := "hsb0"
	bearer := "test-bearer-" + hostname
	if err := store.CreateToken(hostname, hashToken(bearer)); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	hub := sse.NewHub()
	h := RegisterBridge(store, hub, nil)
	return store, h, hostname, bearer
}

func generateEd25519PEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func postRegister(t *testing.T, h http.Handler, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/bridges/register", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRegisterBridge_RejectsAgentNotOnHost(t *testing.T) {
	store, h, hostname, bearer := newBridgeRegisterEnv(t)

	// Host has heartbeat-reported "merlin" only.
	if _, err := store.UpsertHeartbeat(hostname, "Debian", "6.1", 100, "0.6.5", "docker-bare", "b1", nil, []db.Agent{{Name: "merlin"}}, nil); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	pem := generateEd25519PEM(t)
	rr := postRegister(t, h, bearer, RegisterBridgeRequest{
		Agent:     "evil-agent",
		PubkeyPEM: pem,
	})

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resp["error"]; got != "agent_not_on_host" {
		t.Errorf("error = %v, want agent_not_on_host", got)
	}

	// No bridge_pairings row should exist for the rejected attempt.
	rows, err := store.AllBridgePairings()
	if err != nil {
		t.Fatalf("AllBridgePairings: %v", err)
	}
	for _, r := range rows {
		if r.Host == hostname && r.Agent == "evil-agent" {
			t.Errorf("rejected register still wrote a bridge_pairings row: %+v", r)
		}
	}
}

func TestRegisterBridge_AcceptsAgentOnHost(t *testing.T) {
	store, h, hostname, bearer := newBridgeRegisterEnv(t)

	if _, err := store.UpsertHeartbeat(hostname, "Debian", "6.1", 100, "0.6.5", "docker-bare", "b1", nil, []db.Agent{{Name: "merlin"}, {Name: "nimue"}}, nil); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	pem := generateEd25519PEM(t)
	rr := postRegister(t, h, bearer, RegisterBridgeRequest{
		Agent:     "merlin",
		PubkeyPEM: pem,
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	rows, err := store.AllBridgePairings()
	if err != nil {
		t.Fatalf("AllBridgePairings: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Host == hostname && r.Agent == "merlin" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected bridge_pairings row for %s/merlin, got %+v", hostname, rows)
	}
}

func TestRegisterBridge_RejectsHostWithNoAgentsYet(t *testing.T) {
	// Host registered (token exists) but never heartbeated agents — bridge
	// would race the first agent-bearing heartbeat. Strict path rejects;
	// the operator just needs to wait for one heartbeat cycle.
	_, h, _, bearer := newBridgeRegisterEnv(t)

	pem := generateEd25519PEM(t)
	rr := postRegister(t, h, bearer, RegisterBridgeRequest{
		Agent:     "merlin",
		PubkeyPEM: pem,
	})

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}
