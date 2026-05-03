package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/api"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// TestInfoCatalogCoversEveryRoute is the FLEET-80 drift-protection
// guard. It walks the live chi router built by newRouter() and asserts
// every registered (METHOD, path) pair is either:
//   - present in api.EndpointCatalog (with an exact method+path match), or
//   - present in api.ExcludedFromInfo with a documented reason.
//
// If you add a route in router.go, this test fails until you also add a
// matching catalog entry (or an explicit exclusion). That is the entire
// point — /api/info silently lying is the failure mode this test
// prevents.
func TestInfoCatalogCoversEveryRoute(t *testing.T) {
	r := buildTestRouter(t)

	// Index the catalog and exclusions for O(1) lookup.
	cataloged := make(map[string]bool, len(api.EndpointCatalog))
	for _, e := range api.EndpointCatalog {
		cataloged[e.Method+" "+e.Path] = true
	}

	var orphans []string
	walkErr := chi.Walk(r, func(method, route string, handler http.Handler, mw ...func(http.Handler) http.Handler) error {
		// Chi exposes nested routes with their parent prefix joined,
		// which is exactly the path string the catalog uses. Trailing
		// slashes (e.g. "/api/users/") happen for the chi.Router root
		// of a r.Route() block — strip them so they match catalog
		// entries written without the slash.
		path := route
		if len(path) > 1 && path[len(path)-1] == '/' {
			path = path[:len(path)-1]
		}
		key := method + " " + path
		if cataloged[key] {
			return nil
		}
		if _, ok := api.ExcludedFromInfo[key]; ok {
			return nil
		}
		orphans = append(orphans, key)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("chi.Walk: %v", walkErr)
	}

	if len(orphans) > 0 {
		t.Errorf("Routes registered in router.go but missing from api.EndpointCatalog (and not in api.ExcludedFromInfo). Add them or document the exclusion:")
		for _, o := range orphans {
			t.Errorf("  %s", o)
		}
	}
}

// TestInfoCatalogHasNoStaleEntries is the inverse direction: every
// entry in EndpointCatalog must correspond to a real registered route.
// Catches stale catalog entries that linger after a route is removed.
func TestInfoCatalogHasNoStaleEntries(t *testing.T) {
	r := buildTestRouter(t)

	registered := make(map[string]bool)
	chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := route
		if len(path) > 1 && path[len(path)-1] == '/' {
			path = path[:len(path)-1]
		}
		registered[method+" "+path] = true
		return nil
	})

	var stale []string
	for _, e := range api.EndpointCatalog {
		if !registered[e.Method+" "+e.Path] {
			stale = append(stale, e.Method+" "+e.Path)
		}
	}
	if len(stale) > 0 {
		t.Errorf("EndpointCatalog entries with no matching route in router.go (remove them or restore the route):")
		for _, s := range stale {
			t.Errorf("  %s", s)
		}
	}
}

// buildTestRouter wires up newRouter() with throwaway dependencies.
// We don't exercise handlers, only the routing topology, so a fresh
// in-memory SQLite + a real sse.Hub is enough; ocMgr is nil because
// no openclaw-touching handler is invoked during a chi.Walk.
func buildTestRouter(t *testing.T) chi.Router {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "info_test.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})
	return newRouter(&routerDeps{
		store:         store,
		hub:           sse.NewHub(),
		auth:          auth.New(store),
		resetHandlers: auth.NewResetHandlers(store),
		ocMgr:         nil,
	})
}
