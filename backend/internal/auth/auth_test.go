package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/markus-barta/fleetcom/internal/db"
)

// FLEET-161 — RequireSession on missing session must return 401 JSON for
// /api/* paths and 303 redirect for everything else. The 303 caused
// fetch to silently follow to /login HTML, choking client-side
// res.json() with "Unexpected token '<', \"<!DOCTYPE \" is not valid
// JSON" — see openHostCommands toast at v1.1.4.

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "auth-test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	return New(store)
}

func TestRequireSession_MissingCookie_APIPath_Returns401JSON(t *testing.T) {
	a := newTestAuth(t)
	called := false
	handler := a.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest("GET", "/api/hosts/dsc0/commands", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Fatal("next handler should not be invoked when session is missing")
	}
	if got := w.Code; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if body := w.Body.String(); !strings.Contains(body, `"error":"unauthorized"`) {
		t.Errorf("body = %q, want contains \"unauthorized\"", body)
	}
}

func TestRequireSession_MissingCookie_BrowserPath_Redirects(t *testing.T) {
	a := newTestAuth(t)
	handler := a.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Code; got != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", got)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestRequireSession_InvalidCookie_APIPath_Returns401JSON(t *testing.T) {
	a := newTestAuth(t)
	handler := a.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest("GET", "/api/hosts", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "bogus-session-token"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Code; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
