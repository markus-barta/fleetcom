package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/pquerna/otp/totp"

	"encoding/base64"
)

// Me returns the current authenticated user.
func Me(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, u)
	}
}

// ChangePassword handles POST /api/auth/password.
func ChangePassword(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if len(body.NewPassword) < 6 {
			http.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
			return
		}

		full, err := store.GetUserByID(u.ID)
		if err != nil || full == nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !auth.CheckPassword(full.PasswordHash, body.CurrentPassword) {
			http.Error(w, "current password is incorrect", http.StatusUnauthorized)
			return
		}

		hash, err := auth.HashPassword(body.NewPassword)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if err := store.UpdateUserPassword(u.ID, hash); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Invalidate all other sessions (keep current)
		currentToken := auth.GetSessionToken(r)
		sessions, _ := store.ListUserSessions(u.ID)
		for _, s := range sessions {
			if s.Token != currentToken {
				store.DeleteSession(s.Token)
			}
		}

		log.Printf("audit: password_changed user_id=%d ip=%s", u.ID, auth.ClientIP(r))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// TOTPSetup handles GET /api/auth/totp/setup — generates secret + QR.
func TOTPSetup(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		key, err := totp.Generate(totp.GenerateOpts{
			Issuer:      "FleetCom",
			AccountName: u.Email,
		})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Store secret temporarily (not enabled yet)
		if err := store.UpdateUserTOTP(u.ID, key.Secret(), false); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		img, err := key.Image(200, 200)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		writeJSON(w, map[string]string{
			"secret":        key.Secret(),
			"qr_png_base64": base64.StdEncoding.EncodeToString(buf.Bytes()),
			"issuer":        "FleetCom",
			"account":       u.Email,
		})
	}
}

// TOTPEnable handles POST /api/auth/totp/enable — verifies code and activates.
func TOTPEnable(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		full, err := store.GetUserByID(u.ID)
		if err != nil || full == nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if full.TOTPSecret == "" {
			http.Error(w, "run setup first", http.StatusBadRequest)
			return
		}

		if !totp.Validate(body.Code, full.TOTPSecret) {
			http.Error(w, "invalid code", http.StatusUnauthorized)
			return
		}

		if err := store.UpdateUserTOTP(u.ID, full.TOTPSecret, true); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Printf("audit: totp_enabled user_id=%d ip=%s", u.ID, auth.ClientIP(r))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// TOTPDisable handles POST /api/auth/totp/disable — requires password confirmation.
func TOTPDisable(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		full, err := store.GetUserByID(u.ID)
		if err != nil || full == nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !auth.CheckPassword(full.PasswordHash, body.Password) {
			http.Error(w, "incorrect password", http.StatusUnauthorized)
			return
		}

		if err := store.DeleteUserTOTP(u.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Printf("audit: totp_disabled user_id=%d ip=%s", u.ID, auth.ClientIP(r))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// ListSessions handles GET /api/auth/sessions.
func ListSessions(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		sessions, err := store.ListUserSessions(u.ID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Mark current session
		currentToken := auth.GetSessionToken(r)
		for i := range sessions {
			if sessions[i].Token == currentToken {
				sessions[i].Current = true
			}
			sessions[i].Token = "" // never expose tokens
		}

		writeJSON(w, sessions)
	}
}

// RevokeSession handles DELETE /api/auth/sessions/{id}.
func RevokeSession(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.GetUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if err := store.DeleteSessionByID(id, u.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// --- Admin endpoints ---

// ListUsers handles GET /api/users (admin only).
func ListUsers(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := store.ListUsers()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if users == nil {
			users = []db.User{}
		}
		writeJSON(w, users)
	}
}

// CreateUser handles POST /api/users (admin only).
func CreateUser(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if body.Email == "" || body.Password == "" {
			http.Error(w, "email and password required", http.StatusBadRequest)
			return
		}
		if body.Role == "" {
			body.Role = "user"
		}
		if body.Role != "admin" && body.Role != "user" {
			http.Error(w, "role must be admin or user", http.StatusBadRequest)
			return
		}
		if len(body.Password) < 6 {
			http.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
			return
		}

		hash, err := auth.HashPassword(body.Password)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		id, err := store.CreateUser(body.Email, hash, body.Role)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to create user: %v", err), http.StatusConflict)
			return
		}

		admin := auth.GetUser(r)
		log.Printf("audit: user_created id=%d email=%s role=%s by=%d", id, body.Email, body.Role, admin.ID)
		writeJSON(w, map[string]int64{"id": id})
	}
}

// UpdateUserStatus handles PUT /api/users/{id}/status (admin only).
func UpdateUserStatus(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		admin := auth.GetUser(r)
		if admin.ID == id {
			http.Error(w, "cannot change own status", http.StatusBadRequest)
			return
		}

		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		switch body.Status {
		case "active", "inactive", "deleted":
		default:
			http.Error(w, "status must be active, inactive, or deleted", http.StatusBadRequest)
			return
		}

		if err := store.UpdateUserStatus(id, body.Status); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Delete sessions on disable/delete
		if body.Status != "active" {
			store.DeleteUserSessions(id)
		}

		log.Printf("audit: user_status_changed id=%d status=%s by=%d", id, body.Status, admin.ID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// ResetUserTOTP handles POST /api/users/{id}/reset-totp (admin only).
func ResetUserTOTP(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if err := store.DeleteUserTOTP(id); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		admin := auth.GetUser(r)
		log.Printf("audit: totp_reset user_id=%d by=%d", id, admin.ID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// InvalidateUserSessions handles DELETE /api/users/{id}/sessions (admin only).
func InvalidateUserSessions(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if err := store.DeleteUserSessions(id); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		admin := auth.GetUser(r)
		log.Printf("audit: sessions_invalidated user_id=%d by=%d", id, admin.ID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
