package db

import (
	"database/sql"
	"time"
)

type Session struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Token     string `json:"-"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Current   bool   `json:"current,omitempty"`
}

func (s *Store) CreateSession(token string, userID int64, expiresAt time.Time) error {
	_, err := s.DB.Exec(
		`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, expiresAt.UTC().Format(time.RFC3339),
	)
	return err
}

// ValidateSession checks the token and returns the associated user.
// Returns nil if the token is invalid, expired, or the user is inactive/deleted.
func (s *Store) ValidateSession(token string) (*User, error) {
	var u User
	var expiresAt string
	err := s.DB.QueryRow(
		`SELECT u.id, u.email, u.role, u.status, u.totp_enabled, u.created_at, u.avatar, s.expires_at
		 FROM sessions s
		 JOIN users u ON s.user_id = u.id
		 WHERE s.token = ?`,
		token,
	).Scan(&u.ID, &u.Email, &u.Role, &u.Status, &u.TOTPEnabled, &u.CreatedAt, &u.Avatar, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(t) {
		s.DeleteSession(token)
		return nil, nil
	}

	if u.Status != "active" {
		s.DeleteSession(token)
		return nil, nil
	}

	return &u, nil
}

func (s *Store) DeleteSession(token string) {
	s.DB.Exec(`DELETE FROM sessions WHERE token = ?`, token)
}

func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

func (s *Store) ListUserSessions(userID int64) ([]Session, error) {
	rows, err := s.DB.Query(
		`SELECT id, user_id, token, created_at, expires_at FROM sessions
		 WHERE user_id = ? AND expires_at > datetime('now')
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Token, &sess.CreatedAt, &sess.ExpiresAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *Store) DeleteSessionByID(id int64, userID int64) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

func (s *Store) CleanExpiredSessions() {
	s.DB.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
}
