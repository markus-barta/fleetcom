package db

import (
	"database/sql"
	"fmt"
)

type Token struct {
	ID        int64  `json:"id"`
	Hostname  string `json:"hostname"`
	CreatedAt string `json:"created_at"`
}

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

func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.DB.Query(`SELECT id, hostname, created_at FROM tokens ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Hostname, &t.CreatedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}

func (s *Store) DeleteToken(hostname string) error {
	_, err := s.DB.Exec(`DELETE FROM tokens WHERE hostname = ?`, hostname)
	return err
}
