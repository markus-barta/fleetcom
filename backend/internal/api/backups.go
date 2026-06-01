package api

import (
	"encoding/json"
	"net/http"

	"github.com/markus-barta/fleetcom/internal/db"
)

func ListBackups(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := hostsForRequest(store, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		ids := make([]int64, 0, len(hosts))
		for _, h := range hosts {
			ids = append(ids, h.ID)
		}
		backups, err := store.ListBackupsForHosts(ids)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(backups)
	}
}
