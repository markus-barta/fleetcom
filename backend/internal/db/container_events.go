package db

import (
	"fmt"
	"time"
)

func (s *Store) InsertContainerEvent(hostname, containerName, eventType string, exitCode int, oomKilled bool, ts string) error {
	// Look up host ID
	var hostID int64
	err := s.DB.QueryRow(`SELECT id FROM hosts WHERE hostname = ?`, hostname).Scan(&hostID)
	if err != nil {
		return fmt.Errorf("host not found: %w", err)
	}

	oom := 0
	if oomKilled {
		oom = 1
	}

	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}

	_, err = s.DB.Exec(
		`INSERT INTO container_events (host_id, container_name, event_type, exit_code, oom_killed, ts) VALUES (?, ?, ?, ?, ?, ?)`,
		hostID, containerName, eventType, exitCode, oom, ts,
	)
	if err != nil {
		return fmt.Errorf("insert container event: %w", err)
	}
	return nil
}

// PurgeOldContainerEvents deletes events older than the retention window.
func (s *Store) PurgeOldContainerEvents(retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	res, err := s.DB.Exec(`DELETE FROM container_events WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge container events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
