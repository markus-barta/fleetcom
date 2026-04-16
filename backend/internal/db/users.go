package db

import (
	"database/sql"
	"fmt"
	"time"
)

type User struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	TOTPEnabled bool   `json:"totp_enabled"`
	CreatedAt   string `json:"created_at"`
}

// userInternal includes sensitive fields not exposed via JSON.
type userInternal struct {
	User
	PasswordHash string
	TOTPSecret   string
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(email, passwordHash, role string) (int64, error) {
	res, err := s.DB.Exec(
		`INSERT INTO users (email, password_hash, role) VALUES (?, ?, ?)`,
		email, passwordHash, role,
	)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) GetUserByEmail(email string) (*userInternal, error) {
	u := &userInternal{}
	err := s.DB.QueryRow(
		`SELECT id, email, password_hash, role, status, totp_secret, totp_enabled, created_at
		 FROM users WHERE lower(email) = lower(?) AND status != 'deleted'`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status, &u.TOTPSecret, &u.TOTPEnabled, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByID(id int64) (*userInternal, error) {
	u := &userInternal{}
	err := s.DB.QueryRow(
		`SELECT id, email, password_hash, role, status, totp_secret, totp_enabled, created_at
		 FROM users WHERE id = ?`,
		id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status, &u.TOTPSecret, &u.TOTPEnabled, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.DB.Query(
		`SELECT id, email, role, status, totp_enabled, created_at
		 FROM users WHERE status != 'deleted' ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Status, &u.TOTPEnabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) UpdateUserStatus(id int64, status string) error {
	_, err := s.DB.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) UpdateUserPassword(id int64, hash string) error {
	_, err := s.DB.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

func (s *Store) UpdateUserTOTP(id int64, secret string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.DB.Exec(`UPDATE users SET totp_secret = ?, totp_enabled = ? WHERE id = ?`, secret, e, id)
	return err
}

func (s *Store) DeleteUserTOTP(id int64) error {
	return s.UpdateUserTOTP(id, "", false)
}

// TOTP pending tokens

func (s *Store) CreateTOTPPending(token string, userID int64, expiresAt time.Time) error {
	_, err := s.DB.Exec(
		`INSERT INTO totp_pending (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, expiresAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (s *Store) ValidateTOTPPending(token string) (int64, error) {
	var userID int64
	var expiresStr string
	err := s.DB.QueryRow(
		`SELECT user_id, expires_at FROM totp_pending WHERE token = ?`, token,
	).Scan(&userID, &expiresStr)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("invalid token")
	}
	if err != nil {
		return 0, err
	}

	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return 0, fmt.Errorf("parse expiry: %w", err)
	}
	if time.Now().UTC().After(expires) {
		s.DB.Exec(`DELETE FROM totp_pending WHERE token = ?`, token)
		return 0, fmt.Errorf("token expired")
	}
	return userID, nil
}

func (s *Store) DeleteTOTPPending(token string) {
	s.DB.Exec(`DELETE FROM totp_pending WHERE token = ?`, token)
}

func (s *Store) CleanExpiredTOTPPending() {
	s.DB.Exec(`DELETE FROM totp_pending WHERE expires_at < datetime('now')`)
}

// Password reset tokens

func (s *Store) CreatePasswordResetToken(userID int64, tokenHash, ipAddress string, expiresAt time.Time) error {
	_, err := s.DB.Exec(
		`INSERT INTO password_reset_tokens (user_id, token_hash, created_at, expires_at, ip_address)
		 VALUES (?, ?, datetime('now'), ?, ?)`,
		userID, tokenHash, expiresAt.UTC().Format(time.RFC3339), ipAddress,
	)
	return err
}

func (s *Store) ValidatePasswordResetToken(tokenHash string) (int64, error) {
	var userID int64
	var expiresStr string
	var usedAt sql.NullString
	err := s.DB.QueryRow(
		`SELECT user_id, expires_at, used_at FROM password_reset_tokens WHERE token_hash = ?`,
		tokenHash,
	).Scan(&userID, &expiresStr, &usedAt)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("unknown token")
	}
	if err != nil {
		return 0, err
	}
	if usedAt.Valid && usedAt.String != "" {
		return 0, fmt.Errorf("token already used")
	}
	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return 0, fmt.Errorf("parse expiry: %w", err)
	}
	if time.Now().UTC().After(expires) {
		return 0, fmt.Errorf("token expired")
	}
	return userID, nil
}

func (s *Store) UsePasswordResetToken(tokenHash string) error {
	_, err := s.DB.Exec(
		`UPDATE password_reset_tokens SET used_at = datetime('now') WHERE token_hash = ?`,
		tokenHash,
	)
	return err
}

// Host access control

func (s *Store) UserHostIDs(userID int64) (map[int64]bool, error) {
	rows, err := s.DB.Query(`SELECT host_id FROM user_host_access WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func (s *Store) GrantHostAccess(userID, hostID int64) error {
	_, err := s.DB.Exec(
		`INSERT OR IGNORE INTO user_host_access (user_id, host_id) VALUES (?, ?)`,
		userID, hostID,
	)
	return err
}

func (s *Store) RevokeHostAccess(userID, hostID int64) error {
	_, err := s.DB.Exec(
		`DELETE FROM user_host_access WHERE user_id = ? AND host_id = ?`,
		userID, hostID,
	)
	return err
}

func (s *Store) UserHostAccessList(userID int64) ([]Host, error) {
	rows, err := s.DB.Query(
		`SELECT h.id, h.hostname FROM hosts h
		 JOIN user_host_access uha ON h.id = uha.host_id
		 WHERE uha.user_id = ?
		 ORDER BY h.hostname`, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.Hostname); err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (s *Store) ResetPasswordTx(userID int64, newHash, tokenHash string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, newHash, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE password_reset_tokens SET used_at = datetime('now') WHERE token_hash = ?`, tokenHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
