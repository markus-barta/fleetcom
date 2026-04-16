package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie   = "fleetcom_session"
	sessionDuration = 24 * time.Hour
	totpPendingTTL  = 5 * time.Minute
)

type contextKey string

const userContextKey contextKey = "user"

type Auth struct {
	store *db.Store
}

func New(store *db.Store) *Auth {
	return &Auth{store: store}
}

// HashPassword returns a bcrypt hash of the password.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword compares a bcrypt hash with a password.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	email := r.FormValue("email")
	password := r.FormValue("password")

	if ok, retryAfter := AllowAttempt("login", r, email); !ok {
		log.Printf("audit: login_throttled email=%s ip=%s", email, ClientIP(r))
		SetRetryAfter(w, retryAfter)
		return
	}

	user, err := a.store.GetUserByEmail(email)
	if err != nil {
		log.Printf("error: login lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if user == nil || !CheckPassword(user.PasswordHash, password) {
		RecordFailure("login", r, email)
		log.Printf("audit: login_failed email=%s ip=%s", email, ClientIP(r))
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if user.Status != "active" {
		RecordFailure("login", r, email)
		log.Printf("audit: login_blocked email=%s reason=account_%s ip=%s", email, user.Status, ClientIP(r))
		http.Error(w, "account disabled", http.StatusForbidden)
		return
	}

	ResetFailures("login", r, email)

	// If TOTP enabled, go to step 2
	if user.TOTPEnabled {
		token, err := generateToken()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := a.store.CreateTOTPPending(token, user.ID, time.Now().UTC().Add(totpPendingTTL)); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		renderTOTPForm(w, token, "")
		return
	}

	a.createSessionAndRedirect(w, r, user.ID, email)
}

func (a *Auth) HandleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	pendingToken := r.FormValue("totp_token")
	code := r.FormValue("code")

	if ok, retryAfter := AllowAttempt("totp-verify", r, pendingToken); !ok {
		log.Printf("audit: totp_verify_throttled ip=%s", ClientIP(r))
		SetRetryAfter(w, retryAfter)
		return
	}

	userID, err := a.store.ValidateTOTPPending(pendingToken)
	if err != nil {
		RecordFailure("totp-verify", r, pendingToken)
		renderTOTPForm(w, pendingToken, "Invalid or expired session. Please log in again.")
		return
	}

	user, err := a.store.GetUserByID(userID)
	if err != nil || user == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !totp.Validate(code, user.TOTPSecret) {
		RecordFailure("totp-verify", r, pendingToken)
		renderTOTPForm(w, pendingToken, "Invalid code. Please try again.")
		return
	}

	a.store.DeleteTOTPPending(pendingToken)
	ResetFailures("totp-verify", r, pendingToken)
	a.createSessionAndRedirect(w, r, user.ID, user.Email)
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

	log.Printf("audit: logout ip=%s", ClientIP(r))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// RequireSession is middleware that validates the session and puts the user in context.
func (a *Auth) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		user, err := a.store.ValidateSession(cookie.Value)
		if err != nil || user == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdmin is middleware that checks the user has admin role.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := GetUser(r)
		if u == nil || u.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetUser extracts the authenticated user from the request context.
func GetUser(r *http.Request) *db.User {
	u, _ := r.Context().Value(userContextKey).(*db.User)
	return u
}

// GetSessionToken extracts the raw session token from the request cookie.
func GetSessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (a *Auth) createSessionAndRedirect(w http.ResponseWriter, r *http.Request, userID int64, email string) {
	token, err := generateToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := a.store.CreateSession(token, userID, time.Now().UTC().Add(sessionDuration)); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionDuration.Seconds()),
	})

	log.Printf("audit: login_ok email=%s ip=%s", email, ClientIP(r))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SeedAdmin creates the initial admin user if no users exist.
func SeedAdmin(store *db.Store, email, password string) error {
	n, err := store.UserCount()
	if err != nil {
		return fmt.Errorf("check user count: %w", err)
	}
	if n > 0 {
		return nil
	}
	if email == "" || password == "" {
		return nil
	}

	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if _, err := store.CreateUser(email, hash, "admin"); err != nil {
		return fmt.Errorf("create admin: %w", err)
	}
	log.Printf("seeded admin user: %s", email)
	return nil
}

var totpFormTmpl = template.Must(template.New("totp").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FleetCom — Verify</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0f1117;color:#e1e4e8;font-family:system-ui,-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh}
.box{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:32px;width:100%;max-width:360px}
h2{font-size:18px;margin-bottom:20px;color:#f0f6fc;text-align:center}
label{display:block;font-size:13px;margin-bottom:6px;color:#8b949e}
input{width:100%;padding:10px 12px;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e1e4e8;font-size:15px;margin-bottom:16px;text-align:center;letter-spacing:0.5em}
input:focus{outline:none;border-color:#58a6ff}
button{width:100%;padding:10px;background:#238636;border:none;border-radius:6px;color:#fff;font-size:14px;font-weight:600;cursor:pointer}
button:hover{background:#2ea043}
.err{background:#2d1215;border:1px solid #f8514950;color:#f85149;padding:10px;border-radius:6px;margin-bottom:16px;font-size:13px;text-align:center}
.back{display:block;text-align:center;margin-top:16px;color:#58a6ff;font-size:13px;text-decoration:none}
</style>
</head>
<body>
<div class="box">
<h2>Two-Factor Authentication</h2>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form method="POST" action="/login/totp">
<input type="hidden" name="totp_token" value="{{.Token}}">
<label for="code">Authentication Code</label>
<input type="text" id="code" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code" required autofocus>
<button type="submit">Verify</button>
</form>
<a class="back" href="/login">Back to login</a>
</div>
</body>
</html>`))

func renderTOTPForm(w http.ResponseWriter, token, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	totpFormTmpl.Execute(w, struct {
		Token string
		Error string
	}{token, errMsg})
}
