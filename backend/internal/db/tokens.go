package db

import (
	"database/sql"
	"fmt"
)

func (s *Store) ValidateToken(tokenHash string) (string, error) {
	var hostname string
	err := s.DB.QueryRow(`SELECT hostname FROM tokens WHERE token_hash = ?`, tokenHash).Scan(&hostname)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("unknown token")
	}
	if err != nil {
		return "", fmt.Errorf("query token: %w", err)
	}
	return hostname, nil
}

func (s *Store) CreateToken(hostname, tokenHash string) error {
	_, err := s.DB.Exec(`
		INSERT INTO tokens (hostname, token_hash)
		VALUES (?, ?)
		ON CONFLICT(hostname) DO UPDATE SET token_hash = excluded.token_hash
	`, hostname, tokenHash)
	return err
}
