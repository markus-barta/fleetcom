package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
)

type AddHostRequest struct {
	Hostname string `json:"hostname"`
}

type AddHostResponse struct {
	Hostname string `json:"hostname"`
	Token    string `json:"token"`
}

func ListHosts(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := hostsForRequest(store, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hosts)
	}
}

func ListTokens(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokens, err := store.ListTokens()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokens)
	}
}

func AddHost(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req AddHostRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Hostname == "" {
			http.Error(w, "hostname required", http.StatusBadRequest)
			return
		}

		// Generate random token
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		token := hex.EncodeToString(tokenBytes)
		tokenHash := hashToken(token)

		if err := store.CreateToken(req.Hostname, tokenHash); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(AddHostResponse{
			Hostname: req.Hostname,
			Token:    token,
		})
	}
}

func DeleteHost(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := r.URL.Query().Get("hostname")
		if hostname == "" {
			http.Error(w, "hostname required", http.StatusBadRequest)
			return
		}

		if err := store.DeleteToken(hostname); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// RegenerateHostToken issues a fresh bearer token for an existing host,
// replacing the old token_hash in place. The bare token is returned once
// in the response and never shown again. Old token stops working immediately.
func RegenerateHostToken(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostname := chi.URLParam(r, "hostname")
		if hostname == "" {
			http.Error(w, "hostname required", http.StatusBadRequest)
			return
		}

		// Refuse to silently create a new host via the regen path.
		exists, err := store.HostTokenExists(hostname)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "host not found", http.StatusNotFound)
			return
		}

		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		token := hex.EncodeToString(tokenBytes)
		tokenHash := hashToken(token)

		// CreateToken upserts (ON CONFLICT DO UPDATE), so it replaces the
		// existing row's token_hash.
		if err := store.CreateToken(hostname, tokenHash); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Printf("regenerated token for host: %s", hostname)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AddHostResponse{
			Hostname: hostname,
			Token:    token,
		})
	}
}
