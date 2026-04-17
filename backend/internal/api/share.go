package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

type CreateShareRequest struct {
	Label string `json:"label"`
	Hours int    `json:"hours"`
}

func CreateShareLink(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req CreateShareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if req.Hours <= 0 {
			req.Hours = 24
		}
		if req.Hours > 168 { // max 7 days
			req.Hours = 168
		}

		link, err := store.CreateShareLink(u.ID, req.Label, time.Duration(req.Hours)*time.Hour)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(link)
	}
}

func ListShareLinks(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		links, err := store.ListShareLinks()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(links)
	}
}

func DeleteShareLink(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		if err := store.DeleteShareLink(id); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// resolveShareViewer validates the share token and loads the creator so viewers
// inherit the creator's host-access scope. Returns nil if the link is invalid,
// expired, or the creator no longer exists / is disabled.
func resolveShareViewer(store *db.Store, token string) *db.User {
	valid, creatorID, err := store.ValidateShareLink(token)
	if err != nil || !valid || creatorID == 0 {
		return nil
	}
	u, err := store.GetUserByID(creatorID)
	if err != nil || u == nil || u.Status != "active" {
		return nil
	}
	return &u.User
}

// SharedDashboard serves the dashboard for share link viewers
func SharedDashboard(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if resolveShareViewer(store, token) == nil {
			http.Error(w, "Link expired or invalid", http.StatusForbidden)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	}
}

// SharedEvents serves SSE for share link viewers, scoped to the creator's hosts.
func SharedEvents(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		viewer := resolveShareViewer(store, token)
		if viewer == nil {
			http.Error(w, "Link expired or invalid", http.StatusForbidden)
			return
		}
		// Inject the creator as the "user" for this request so Events() filters
		// host lists, host-configs, and the ignored set the same way it does
		// for an authenticated session belonging to the creator.
		r2 := r.WithContext(auth.WithUser(r.Context(), viewer))
		Events(store, hub)(w, r2)
	}
}
