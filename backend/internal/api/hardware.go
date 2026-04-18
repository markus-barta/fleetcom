package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
)

// HostHardware returns the full hardware/metadata payload for one host.
// Respects per-user host access — regular users get 404 on hosts they
// haven't been granted access to (same shape as "not found").
func HostHardware(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := chi.URLParam(r, "hostname")
		if hostname == "" {
			http.Error(w, "hostname required", http.StatusBadRequest)
			return
		}

		allowed, err := hostsForRequest(store, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		ok := false
		for _, h := range allowed {
			if h.Hostname == hostname {
				ok = true
				break
			}
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		hw, err := store.HostHardware(hostname)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if hw == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hw)
	}
}
