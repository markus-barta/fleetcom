package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

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
