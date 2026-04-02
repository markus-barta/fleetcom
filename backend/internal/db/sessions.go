package db

import (
	"database/sql"
	"time"
)

func (s *Store) CreateSession(token string, expiresAt time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO sessions (token, expires_at) VALUES (?, ?)`,
		token, expiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) ValidateSession(token string) (bool, error) {
	var expiresAt string
	err := s.DB.QueryRow(`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&expiresAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false, err
	}

	if time.Now().UTC().After(t) {
		s.DeleteSession(token)
		return false, nil
	}

	return true, nil
}

func (s *Store) DeleteSession(token string) {
	s.DB.Exec(`DELETE FROM sessions WHERE token = ?`, token)
}

func (s *Store) CleanExpiredSessions() {
	s.DB.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
}
