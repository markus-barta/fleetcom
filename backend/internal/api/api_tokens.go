package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
)

// FLEET-79: self-service handlers for user-issued API tokens. Mounted
// under the existing /api/auth/* group, so they require an active
// browser session + TOTP — tokens can only be minted from a verified
// human session, never from another token.

const (
	// 32 random bytes → 64 hex chars after the prefix.
	apiTokenRandomBytes = 32
	// Soft caps to keep abuse / accidental-paste payloads contained.
	apiTokenLabelMaxLen = 128
	apiTokenMaxScopes   = 16
	// Per-user token cap. Enough headroom for one-token-per-machine
	// across a typical fleet, low enough that runaway scripts get
	// noticed.
	apiTokenPerUserMax = 50
)

// ListAPITokens handles GET /api/auth/api-tokens — returns the caller's
// own tokens, never their hashes or plaintext values.
func ListAPITokens(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		toks, err := store.ListUserAPITokens(u.ID)
		if err != nil {
			log.Printf("error: list api tokens user_id=%d: %v", u.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if toks == nil {
			toks = []db.APIToken{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toks)
	}
}

type createAPITokenRequest struct {
	Label     string   `json:"label"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"` // RFC3339; nil/"" = never expires
}

type createAPITokenResponse struct {
	db.APIToken
	// Token is the plaintext value, returned exactly once at creation
	// time and never persisted in plaintext anywhere on the server.
	Token string `json:"token"`
}

// CreateAPIToken handles POST /api/auth/api-tokens — mints a new token
// and returns the plaintext value once.
func CreateAPIToken(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req createAPITokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		req.Label = strings.TrimSpace(req.Label)
		if req.Label == "" {
			http.Error(w, "label is required", http.StatusBadRequest)
			return
		}
		if len(req.Label) > apiTokenLabelMaxLen {
			http.Error(w, "label too long", http.StatusBadRequest)
			return
		}
		if len(req.Scopes) == 0 {
			http.Error(w, "at least one scope is required", http.StatusBadRequest)
			return
		}
		if len(req.Scopes) > apiTokenMaxScopes {
			http.Error(w, "too many scopes", http.StatusBadRequest)
			return
		}
		// De-dup + validate against allowlist.
		seen := make(map[string]bool, len(req.Scopes))
		clean := make([]string, 0, len(req.Scopes))
		for _, s := range req.Scopes {
			if seen[s] {
				continue
			}
			if !auth.IsValidAPIScope(s) {
				http.Error(w, "unknown scope: "+s, http.StatusBadRequest)
				return
			}
			seen[s] = true
			clean = append(clean, s)
		}

		var expiresAt *time.Time
		if req.ExpiresAt != nil && *req.ExpiresAt != "" {
			t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
			if err != nil {
				http.Error(w, "expires_at must be RFC3339", http.StatusBadRequest)
				return
			}
			if t.Before(time.Now().UTC().Add(time.Minute)) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			t = t.UTC()
			expiresAt = &t
		}

		// Per-user cap — enforced in user-space rather than via a DB
		// constraint so the error message is friendly.
		existing, err := store.ListUserAPITokens(u.ID)
		if err != nil {
			log.Printf("error: list api tokens user_id=%d: %v", u.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if len(existing) >= apiTokenPerUserMax {
			http.Error(w, "token limit reached; revoke an existing token first", http.StatusConflict)
			return
		}

		// Generate the token. 32 random bytes → 64 hex chars.
		raw := make([]byte, apiTokenRandomBytes)
		if _, err := rand.Read(raw); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		hexBody := hex.EncodeToString(raw)
		plaintext := auth.APITokenPrefix + hexBody
		// Display prefix = literal prefix + first 8 hex chars of the body.
		// Stored separately so the UI can render "fleet_pat_a3f12bcd…" in
		// lists without ever holding the full token.
		displayPrefix := auth.APITokenPrefix + hexBody[:8]

		id, err := store.CreateAPIToken(u.ID, auth.HashAPIToken(plaintext), displayPrefix, req.Label, clean, expiresAt)
		if err != nil {
			log.Printf("error: create api token user_id=%d: %v", u.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		expiresLog := "never"
		if expiresAt != nil {
			expiresLog = expiresAt.Format(time.RFC3339)
		}
		log.Printf("audit: api_token_created user_id=%d email=%s token_id=%d label=%q scopes=%v expires=%s ip=%s",
			u.ID, u.Email, id, req.Label, clean, expiresLog, auth.ClientIP(r))

		resp := createAPITokenResponse{
			APIToken: db.APIToken{
				ID:        id,
				Label:     req.Label,
				Prefix:    displayPrefix,
				Scopes:    clean,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			Token: plaintext,
		}
		if expiresAt != nil {
			resp.APIToken.ExpiresAt = expiresAt.Format(time.RFC3339)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(resp)
	}
}

// RevokeAPIToken handles DELETE /api/auth/api-tokens/{id}.
func RevokeAPIToken(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "bad token id", http.StatusBadRequest)
			return
		}
		if err := store.RevokeAPIToken(id, u.ID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			log.Printf("error: revoke api token user_id=%d token_id=%d: %v", u.ID, id, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		log.Printf("audit: api_token_revoked user_id=%d email=%s token_id=%d ip=%s",
			u.ID, u.Email, id, auth.ClientIP(r))
		w.WriteHeader(http.StatusNoContent)
	}
}
