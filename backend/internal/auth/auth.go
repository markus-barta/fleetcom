package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/pquerna/otp/totp"
)

const (
	sessionCookie  = "fleetcom_session"
	sessionMaxAge  = 7 * 24 * time.Hour
)

type Auth struct {
	store        *db.Store
	passwordHash string
	totpSecret   string
}

func New(store *db.Store) *Auth {
	return &Auth{
		store:        store,
		passwordHash: os.Getenv("FLEETCOM_PASSWORD_HASH"),
		totpSecret:   os.Getenv("FLEETCOM_TOTP_SECRET"),
	}
}

func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	password := r.FormValue("password")
	code := r.FormValue("totp")

	if !a.checkPassword(password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if !a.checkTOTP(code) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := generateToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().UTC().Add(sessionMaxAge)
	if err := a.store.CreateSession(token, expiresAt); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	secure := os.Getenv("FLEETCOM_INSECURE") == ""
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionMaxAge.Seconds()),
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err == nil {
		a.store.DeleteSession(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *Auth) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		valid, err := a.store.ValidateSession(cookie.Value)
		if err != nil || !valid {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (a *Auth) checkPassword(password string) bool {
	h := sha256.Sum256([]byte(password))
	got := hex.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.passwordHash)) == 1
}

func (a *Auth) checkTOTP(code string) bool {
	if a.totpSecret == "" {
		return true // TOTP not configured, skip
	}
	return totp.Validate(code, a.totpSecret)
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
