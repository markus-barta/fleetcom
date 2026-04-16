package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const (
	passwordResetTTL    = 60 * time.Minute
	passwordResetMinLen = 8
)

type ResetHandlers struct {
	store *db.Store
}

func NewResetHandlers(store *db.Store) *ResetHandlers {
	return &ResetHandlers{store: store}
}

// HandleForgotPassword handles POST /forgot-password.
// Always responds the same way to prevent user enumeration.
func (h *ResetHandlers) HandleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	email := r.FormValue("email")
	ip := ClientIP(r)

	if ok, retryAfter := AllowAttempt("forgot", r, email); !ok {
		log.Printf("audit: forgot_throttled email=%s ip=%s", email, ip)
		SetRetryAfter(w, retryAfter)
		return
	}

	// Always show the same page regardless of whether the email exists
	defer func() {
		renderForgotSent(w)
	}()

	user, err := h.store.GetUserByEmail(email)
	if err != nil {
		log.Printf("error: forgot password lookup: %v", err)
		return
	}
	if user == nil || user.Status != "active" {
		RecordFailure("forgot", r, email)
		return
	}

	rawToken, err := generateResetToken()
	if err != nil {
		log.Printf("error: generate reset token: %v", err)
		return
	}

	tokenHash := hashResetToken(rawToken)
	expiresAt := time.Now().UTC().Add(passwordResetTTL)

	if err := h.store.CreatePasswordResetToken(user.ID, tokenHash, ip, expiresAt); err != nil {
		log.Printf("error: create reset token: %v", err)
		return
	}

	resetLink := fmt.Sprintf("%s/reset/%s", appBaseURL(), rawToken)
	if err := SendResetEmail(user.Email, resetLink); err != nil {
		log.Printf("error: send reset email: %v", err)
	}
}

// HandleResetForm handles GET /reset/{token} — renders the new password form.
func (h *ResetHandlers) HandleResetForm(w http.ResponseWriter, r *http.Request) {
	rawToken := chi.URLParam(r, "token")
	tokenHash := hashResetToken(rawToken)

	if _, err := h.store.ValidatePasswordResetToken(tokenHash); err != nil {
		renderResetForm(w, "", "This reset link is invalid or has expired. Please request a new one.")
		return
	}

	renderResetForm(w, rawToken, "")
}

// HandleResetPassword handles POST /reset — validates token and sets new password.
func (h *ResetHandlers) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	rawToken := r.FormValue("token")
	newPassword := r.FormValue("password")
	ip := ClientIP(r)

	if ok, retryAfter := AllowAttempt("reset", r, ip); !ok {
		SetRetryAfter(w, retryAfter)
		return
	}

	if len(newPassword) < passwordResetMinLen {
		renderResetForm(w, rawToken, fmt.Sprintf("Password must be at least %d characters.", passwordResetMinLen))
		return
	}

	tokenHash := hashResetToken(rawToken)
	userID, err := h.store.ValidatePasswordResetToken(tokenHash)
	if err != nil {
		RecordFailure("reset", r, ip)
		renderResetForm(w, "", "This reset link is invalid or has expired. Please request a new one.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.store.ResetPasswordTx(userID, string(hash), tokenHash); err != nil {
		log.Printf("error: reset password tx: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ResetFailures("reset", r, ip)
	log.Printf("audit: password_reset user_id=%d ip=%s", userID, ip)
	http.Redirect(w, r, "/login?reset=ok", http.StatusSeeOther)
}

func generateResetToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

func hashResetToken(rawToken string) string {
	h := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(h[:])
}

var forgotSentTmpl = template.Must(template.New("forgot-sent").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FleetCom — Check Your Email</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0f1117;color:#e1e4e8;font-family:system-ui,-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh}
.box{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:32px;width:100%;max-width:400px;text-align:center}
h2{font-size:18px;margin-bottom:12px;color:#f0f6fc}
p{font-size:14px;color:#8b949e;line-height:1.5;margin-bottom:20px}
a{color:#58a6ff;font-size:13px;text-decoration:none}
</style>
</head>
<body>
<div class="box">
<h2>Check Your Email</h2>
<p>If an account exists with that email, we've sent a password reset link. The link expires in 60 minutes.</p>
<a href="/login">Back to login</a>
</div>
</body>
</html>`))

func renderForgotSent(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	forgotSentTmpl.Execute(w, nil)
}

var resetFormTmpl = template.Must(template.New("reset").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FleetCom — Reset Password</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0f1117;color:#e1e4e8;font-family:system-ui,-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh}
.box{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:32px;width:100%;max-width:360px}
h2{font-size:18px;margin-bottom:20px;color:#f0f6fc;text-align:center}
label{display:block;font-size:13px;margin-bottom:6px;color:#8b949e}
input{width:100%;padding:10px 12px;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e1e4e8;font-size:15px;margin-bottom:16px}
input:focus{outline:none;border-color:#58a6ff}
button{width:100%;padding:10px;background:#238636;border:none;border-radius:6px;color:#fff;font-size:14px;font-weight:600;cursor:pointer}
button:hover{background:#2ea043}
.err{background:#2d1215;border:1px solid #f8514950;color:#f85149;padding:10px;border-radius:6px;margin-bottom:16px;font-size:13px;text-align:center}
a{display:block;text-align:center;margin-top:16px;color:#58a6ff;font-size:13px;text-decoration:none}
</style>
</head>
<body>
<div class="box">
<h2>Reset Password</h2>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
{{if .Token}}
<form method="POST" action="/reset">
<input type="hidden" name="token" value="{{.Token}}">
<label for="password">New Password (min 8 characters)</label>
<input type="password" id="password" name="password" required minlength="8" autofocus>
<button type="submit">Set New Password</button>
</form>
{{end}}
<a href="/login">Back to login</a>
</div>
</body>
</html>`))

func renderResetForm(w http.ResponseWriter, token, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	resetFormTmpl.Execute(w, struct {
		Token string
		Error string
	}{token, errMsg})
}
