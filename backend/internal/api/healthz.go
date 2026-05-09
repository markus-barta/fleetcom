package api

import (
	"context"
	"net/http"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
)

// Healthz returns 200/ok only when the DB is responsive within 2s.
//
// FLEET-138: a static "ok" responder lets the v1.0.10 deadlock outage
// (a single connection held forever, every other handler blocked) hide
// from external healthchecks while user traffic was 100% broken. The
// short timeout means a stuck pool surfaces as 503 instead of a hang;
// healthchecks can act on it (alert, restart, drain).
func Healthz(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		var n int
		if err := store.DB.QueryRowContext(ctx, `SELECT 1`).Scan(&n); err != nil {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("db unhealthy: " + err.Error()))
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
