package db

import (
	"fmt"
	"strconv"
)

// GetSetting returns the value for a key, or fallback if not found.
func (s *Store) GetSetting(key, fallback string) (string, error) {
	var val string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err != nil {
		return fallback, nil //nolint:nilerr // missing key is not an error
	}
	return val, nil
}

// SetSetting upserts a key-value pair.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// AllSettings returns every row as a map.
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.DB.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("query settings: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// HeartbeatInterval returns the configured interval in seconds (default 60).
func (s *Store) HeartbeatInterval() int {
	val, _ := s.GetSetting("heartbeat_interval", "60")
	n, err := strconv.Atoi(val)
	if err != nil || n < 10 {
		return 60
	}
	return n
}
