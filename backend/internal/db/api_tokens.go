package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// APIToken is the user-facing view of a row in user_api_tokens.
// The plaintext token value is never returned by any read method — it
// exists only in the response of CreateAPIToken (returned to the caller
// once and discarded immediately).
type APIToken struct {
	ID         int64    `json:"id"`
	UserID     int64    `json:"user_id,omitempty"`
	Label      string   `json:"label"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
	CreatedAt  string   `json:"created_at"`
}

// CreateAPIToken inserts a new token row. tokenHash is the SHA-256 hex of
// the full plaintext token. The plaintext is not stored anywhere; the
// caller is responsible for returning it to the user exactly once.
// expiresAt may be nil (token never expires).
func (s *Store) CreateAPIToken(userID int64, tokenHash, prefix, label string, scopes []string, expiresAt *time.Time) (int64, error) {
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return 0, fmt.Errorf("marshal scopes: %w", err)
	}
	var expiresStr sql.NullString
	if expiresAt != nil {
		expiresStr = sql.NullString{String: expiresAt.UTC().Format(time.RFC3339), Valid: true}
	}
	res, err := s.DB.Exec(
		`INSERT INTO user_api_tokens (user_id, token_hash, prefix, label, scopes, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		userID, tokenHash, prefix, label, string(scopesJSON), expiresStr,
	)
	if err != nil {
		return 0, fmt.Errorf("insert api token: %w", err)
	}
	return res.LastInsertId()
}

// ListUserAPITokens returns the caller's tokens, ordered most-recent first.
// Revoked tokens are excluded. The token_hash is never exposed.
func (s *Store) ListUserAPITokens(userID int64) ([]APIToken, error) {
	rows, err := s.DB.Query(
		`SELECT id, label, prefix, scopes, last_used_at, expires_at, created_at
		 FROM user_api_tokens
		 WHERE user_id = ? AND revoked_at IS NULL
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	var out []APIToken
	for rows.Next() {
		var t APIToken
		var scopesJSON string
		var lastUsed, expires sql.NullString
		if err := rows.Scan(&t.ID, &t.Label, &t.Prefix, &scopesJSON, &lastUsed, &expires, &t.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &t.Scopes); err != nil {
			t.Scopes = []string{}
		}
		if lastUsed.Valid {
			t.LastUsedAt = lastUsed.String
		}
		if expires.Valid {
			t.ExpiresAt = expires.String
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAPITokenByHash looks up a token by its SHA-256 hash and returns both
// the token row and the owning user. Returns (nil, nil, nil) if not found.
// Callers are responsible for checking revoked_at, expires_at, and user
// status — this method does NOT filter on those (so the middleware can log
// distinct failure reasons in audit entries).
func (s *Store) GetAPITokenByHash(tokenHash string) (*APIToken, *User, error) {
	var t APIToken
	var scopesJSON string
	var lastUsed, expires, revoked sql.NullString
	var u User
	var avatar sql.NullString
	err := s.DB.QueryRow(
		`SELECT t.id, t.user_id, t.label, t.prefix, t.scopes, t.last_used_at, t.expires_at, t.revoked_at, t.created_at,
		        u.id, u.email, u.role, u.status, u.totp_enabled, u.created_at, u.avatar
		 FROM user_api_tokens t
		 JOIN users u ON u.id = t.user_id
		 WHERE t.token_hash = ?`,
		tokenHash,
	).Scan(
		&t.ID, &t.UserID, &t.Label, &t.Prefix, &scopesJSON, &lastUsed, &expires, &revoked, &t.CreatedAt,
		&u.ID, &u.Email, &u.Role, &u.Status, &u.TOTPEnabled, &u.CreatedAt, &avatar,
	)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get api token: %w", err)
	}
	if err := json.Unmarshal([]byte(scopesJSON), &t.Scopes); err != nil {
		t.Scopes = []string{}
	}
	if lastUsed.Valid {
		t.LastUsedAt = lastUsed.String
	}
	if expires.Valid {
		t.ExpiresAt = expires.String
	}
	if revoked.Valid {
		t.RevokedAt = revoked.String
	}
	if avatar.Valid {
		u.Avatar = avatar.String
	}
	return &t, &u, nil
}

// RevokeAPIToken marks a token revoked. Owner-only: an authenticated user
// can only revoke their own tokens. Returns sql.ErrNoRows if the token
// either doesn't exist or doesn't belong to this user — both surface to
// the caller as a 404 with no information leak.
func (s *Store) RevokeAPIToken(tokenID, userID int64) error {
	res, err := s.DB.Exec(
		`UPDATE user_api_tokens
		 SET revoked_at = ?
		 WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), tokenID, userID,
	)
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// TouchAPITokenLastUsed updates last_used_at. The middleware throttles
// calls (one per 60s per token) so this is allowed to do an unconditional
// write — the throttling is the caller's responsibility, not the DB's.
func (s *Store) TouchAPITokenLastUsed(tokenID int64, t time.Time) error {
	_, err := s.DB.Exec(
		`UPDATE user_api_tokens SET last_used_at = ? WHERE id = ?`,
		t.UTC().Format(time.RFC3339), tokenID,
	)
	return err
}
