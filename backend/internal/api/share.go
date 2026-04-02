package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

type CreateShareRequest struct {
	Label string `json:"label"`
	Hours int    `json:"hours"`
}

func CreateShareLink(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		link, err := store.CreateShareLink(req.Label, time.Duration(req.Hours)*time.Hour)
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

// SharedDashboard serves the dashboard for share link viewers
func SharedDashboard(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		valid, err := store.ValidateShareLink(token)
		if err != nil || !valid {
			http.Error(w, "Link expired or invalid", http.StatusForbidden)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	}
}

// SharedEvents serves SSE for share link viewers
func SharedEvents(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		valid, err := store.ValidateShareLink(token)
		if err != nil || !valid {
			http.Error(w, "Link expired or invalid", http.StatusForbidden)
			return
		}
		// Delegate to the regular SSE handler
		Events(store, hub)(w, r)
	}
}
