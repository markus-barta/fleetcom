package auth

import (
	"fmt"
	"log"
	"net/smtp"
	"os"
)

func smtpHost() string   { return os.Getenv("SMTP_HOST") }
func smtpPort() string   { return envOr("SMTP_PORT", "587") }
func smtpUser() string   { return os.Getenv("SMTP_USER") }
func smtpPass() string   { return os.Getenv("SMTP_PASS") }
func smtpFrom() string   { return envOr("SMTP_FROM", "noreply@fleetcom.local") }
func appBaseURL() string { return envOr("APP_BASE_URL", "http://localhost:8090") }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// SendResetEmail sends a password reset email. If SMTP is not configured, logs the link.
func SendResetEmail(toEmail, resetLink string) error {
	host := smtpHost()
	if host == "" {
		log.Printf("[password-reset] SMTP_HOST unset — reset link for %s: %s", toEmail, resetLink)
		return nil
	}

	from := smtpFrom()
	subject := "FleetCom password reset"
	body := fmt.Sprintf(
		"You (or someone using your email) requested a password reset for FleetCom.\n\n"+
			"Open this link within 60 minutes to choose a new password:\n\n"+
			"  %s\n\n"+
			"If you did not request this, ignore this email. Your password\n"+
			"will not change unless you follow the link.\n",
		resetLink,
	)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s", from, toEmail, subject, body)

	addr := host + ":" + smtpPort()
	var a smtp.Auth
	if user := smtpUser(); user != "" {
		a = smtp.PlainAuth("", user, smtpPass(), host)
	}

	if err := smtp.SendMail(addr, a, from, []string{toEmail}, []byte(msg)); err != nil {
		return fmt.Errorf("send reset email: %w", err)
	}
	log.Printf("[password-reset] email sent to %s", toEmail)
	return nil
}
