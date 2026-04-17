package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

type ShareLink struct {
	ID        int64  `json:"id"`
	Token     string `json:"token"`
	Label     string `json:"label"`
	UserID    int64  `json:"user_id"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Store) CreateShareLink(userID int64, label string, duration time.Duration) (*ShareLink, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(b)
	expiresAt := time.Now().UTC().Add(duration).Format(time.RFC3339)

	_, err := s.DB.Exec(
		`INSERT INTO share_links (token, label, user_id, expires_at) VALUES (?, ?, ?, ?)`,
		token, label, userID, expiresAt,
	)
	if err != nil {
		return nil, err
	}

	return &ShareLink{Token: token, Label: label, UserID: userID, ExpiresAt: expiresAt}, nil
}

// ValidateShareLink returns whether the link exists and is unexpired, along with
// the creator's user_id so viewers can inherit the creator's access scope.
func (s *Store) ValidateShareLink(token string) (valid bool, userID int64, err error) {
	var expiresAt string
	err = s.DB.QueryRow(
		`SELECT expires_at, user_id FROM share_links WHERE token = ?`, token,
	).Scan(&expiresAt, &userID)
	if err == sql.ErrNoRows {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}

	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false, 0, err
	}

	if time.Now().UTC().After(t) {
		s.DB.Exec(`DELETE FROM share_links WHERE token = ?`, token)
		return false, 0, nil
	}

	return true, userID, nil
}

func (s *Store) ListShareLinks() ([]ShareLink, error) {
	// Clean expired first
	s.DB.Exec(`DELETE FROM share_links WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))

	rows, err := s.DB.Query(`SELECT id, token, label, user_id, created_at, expires_at FROM share_links ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []ShareLink
	for rows.Next() {
		var l ShareLink
		if err := rows.Scan(&l.ID, &l.Token, &l.Label, &l.UserID, &l.CreatedAt, &l.ExpiresAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, nil
}

func (s *Store) DeleteShareLink(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM share_links WHERE id = ?`, id)
	return err
}
