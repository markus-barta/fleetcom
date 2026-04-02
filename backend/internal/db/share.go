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
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Store) CreateShareLink(label string, duration time.Duration) (*ShareLink, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(b)
	expiresAt := time.Now().UTC().Add(duration).Format(time.RFC3339)

	_, err := s.DB.Exec(`INSERT INTO share_links (token, label, expires_at) VALUES (?, ?, ?)`,
		token, label, expiresAt)
	if err != nil {
		return nil, err
	}

	return &ShareLink{Token: token, Label: label, ExpiresAt: expiresAt}, nil
}

func (s *Store) ValidateShareLink(token string) (bool, error) {
	var expiresAt string
	err := s.DB.QueryRow(`SELECT expires_at FROM share_links WHERE token = ?`, token).Scan(&expiresAt)
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
		s.DB.Exec(`DELETE FROM share_links WHERE token = ?`, token)
		return false, nil
	}

	return true, nil
}

func (s *Store) ListShareLinks() ([]ShareLink, error) {
	// Clean expired first
	s.DB.Exec(`DELETE FROM share_links WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))

	rows, err := s.DB.Query(`SELECT id, token, label, created_at, expires_at FROM share_links ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []ShareLink
	for rows.Next() {
		var l ShareLink
		if err := rows.Scan(&l.ID, &l.Token, &l.Label, &l.CreatedAt, &l.ExpiresAt); err != nil {
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
