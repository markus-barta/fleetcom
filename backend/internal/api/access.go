package api

import (
	"net/http"

	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
)

// hostsForRequest returns hosts filtered by the user's access.
// Admins see all hosts; regular users see only hosts they've been granted access to.
func hostsForRequest(store *db.Store, r *http.Request) ([]db.Host, error) {
	u := auth.GetUser(r)
	if u != nil && u.Role == "admin" {
		return store.AllHosts()
	}
	if u != nil {
		return store.HostsForUser(u.ID)
	}
	return nil, nil
}

// filterHostConfigs returns only configs for hosts the user can access.
func filterHostConfigs(cfgs map[string]db.HostConfig, allowedHosts []db.Host) map[string]db.HostConfig {
	allowed := make(map[string]bool)
	for _, h := range allowedHosts {
		allowed[h.Hostname] = true
	}
	filtered := make(map[string]db.HostConfig)
	for k, v := range cfgs {
		if allowed[k] {
			filtered[k] = v
		}
	}
	return filtered
}
