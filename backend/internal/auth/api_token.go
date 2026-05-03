package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/markus-barta/fleetcom/internal/db"
)

// FLEET-79: user-issued API tokens for read-only programmatic access.
//
// Tokens are formatted as "fleet_pat_<64 hex chars>" — the prefix lets
// secret scanners recognize them and gives operators a paste-detection
// hook. The full token is hashed with SHA-256 before storage; only the
// hash and the short prefix ("fleet_pat_<first 8 hex>") ever land in the
// database. The plaintext is returned to the user exactly once at
// creation time and is otherwise unrecoverable.

// APITokenPrefix is the literal namespace prefix every API token starts
// with. Exported so info.go (FLEET-80) and tests can reference it.
const APITokenPrefix = "fleet_pat_"

// APITokenScopes is the v1 scope allowlist. Token creation requests are
// validated against this list — an unknown scope is rejected at the API
// layer rather than silently accepted. Adding a scope here is a public
// API change (clients see it in /api/info via FLEET-80).
var APITokenScopes = []string{
	"read:hosts",
	"read:agents",
	"read:hardware",
	"read:info",
}

// IsValidAPIScope reports whether a scope string is in the v1 allowlist.
func IsValidAPIScope(s string) bool {
	for _, v := range APITokenScopes {
		if v == s {
			return true
		}
	}
	return false
}

// HashAPIToken returns the SHA-256 hex of a token. Used both for storage
// and for lookup — never store or log the plaintext.
func HashAPIToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// APITokenLogPrefix returns the loggable prefix slice of a token —
// "fleet_pat_xxxx" (prefix + first 4 hex chars). Logging the full token
// is forbidden; this helper exists so audit entries always go through a
// safe redactor.
func APITokenLogPrefix(token string) string {
	const safeLen = len(APITokenPrefix) + 4
	if len(token) < safeLen {
		return APITokenPrefix
	}
	return token[:safeLen]
}

// apiTokenAuthFlag is set on the request context by MaybeAPIToken on
// successful authentication. RequireSession and RequireTOTP both check
// this flag and short-circuit when set, so a single chain
//
//	MaybeAPIToken(scope) → RequireSession → RequireTOTP → handler
//
// works for both auth methods without per-handler branching.
const apiTokenAuthFlag contextKey = "api_token_auth"

// IsAPITokenAuth reports whether the current request was authenticated
// via an API token (vs. a browser session). Read by RequireSession and
// RequireTOTP to skip cookie / TOTP checks.
func IsAPITokenAuth(r *http.Request) bool {
	v, _ := r.Context().Value(apiTokenAuthFlag).(bool)
	return v
}

// MaybeAPIToken authenticates a request via an "Authorization: Bearer
// fleet_pat_…" header. On success it attaches the owning user to the
// request context (via WithUser) and sets a flag that downstream
// session/TOTP middleware honor. On a malformed-or-missing token it
// falls through unchanged so browser auth can take over. On a
// well-formed but invalid token it returns 401 without falling through —
// presenting a token is an explicit auth attempt and silently downgrading
// to "try the cookie" would be surprising.
//
// requiredScope must be one of APITokenScopes (or "" to skip the scope
// check, currently unused).
func MaybeAPIToken(store *db.Store, requiredScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if !strings.HasPrefix(raw, APITokenPrefix) {
				next.ServeHTTP(w, r)
				return
			}

			prefix := APITokenLogPrefix(raw)
			if ok, retry := AllowAttempt("api-token-auth", r, prefix); !ok {
				log.Printf("audit: api_token_auth_throttled prefix=%s ip=%s", prefix, ClientIP(r))
				SetRetryAfter(w, retry)
				return
			}

			tok, user, err := store.GetAPITokenByHash(HashAPIToken(raw))
			if err != nil {
				log.Printf("error: api_token_lookup: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if tok == nil || user == nil {
				RecordFailure("api-token-auth", r, prefix)
				log.Printf("audit: api_token_auth_failed prefix=%s ip=%s reason=unknown", prefix, ClientIP(r))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if tok.RevokedAt != "" {
				RecordFailure("api-token-auth", r, prefix)
				log.Printf("audit: api_token_auth_failed prefix=%s ip=%s reason=revoked", prefix, ClientIP(r))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if tok.ExpiresAt != "" {
				if t, err := time.Parse(time.RFC3339, tok.ExpiresAt); err == nil && time.Now().UTC().After(t) {
					RecordFailure("api-token-auth", r, prefix)
					log.Printf("audit: api_token_auth_failed prefix=%s ip=%s reason=expired", prefix, ClientIP(r))
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			if user.Status != "active" {
				RecordFailure("api-token-auth", r, prefix)
				log.Printf("audit: api_token_auth_failed prefix=%s ip=%s reason=user_inactive", prefix, ClientIP(r))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if requiredScope != "" {
				hasScope := false
				for _, s := range tok.Scopes {
					if s == requiredScope {
						hasScope = true
						break
					}
				}
				if !hasScope {
					// Not a brute-force vector (token is valid, just under-scoped),
					// so don't count toward the rate-limit budget. Still audit-logged.
					log.Printf("audit: api_token_auth_failed prefix=%s ip=%s reason=missing_scope want=%s", prefix, ClientIP(r), requiredScope)
					http.Error(w, "forbidden: token lacks scope "+requiredScope, http.StatusForbidden)
					return
				}
			}

			// Throttled last_used update — at most one DB write per token per minute.
			shouldTouch := tok.LastUsedAt == ""
			if !shouldTouch {
				if lu, err := time.Parse(time.RFC3339, tok.LastUsedAt); err == nil && time.Since(lu) > time.Minute {
					shouldTouch = true
				}
			}
			if shouldTouch {
				if err := store.TouchAPITokenLastUsed(tok.ID, time.Now().UTC()); err != nil {
					log.Printf("warn: touch api_token last_used (token_id=%d): %v", tok.ID, err)
				}
			}

			ResetFailures("api-token-auth", r, prefix)
			ctx := WithUser(r.Context(), user)
			ctx = context.WithValue(ctx, apiTokenAuthFlag, true)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the raw value of an "Authorization: Bearer …"
// header, or "" if not present.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
