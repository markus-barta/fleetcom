package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Backup is the latest backup-observability state for one host/service.
type Backup struct {
	ID            int64    `json:"id,omitempty"`
	HostID        int64    `json:"host_id,omitempty"`
	Hostname      string   `json:"hostname,omitempty"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Status        string   `json:"status"`
	ContainerName string   `json:"container_name,omitempty"`
	LastSuccessAt string   `json:"last_success_at,omitempty"`
	LastCheckedAt string   `json:"last_checked_at,omitempty"`
	SnapshotID    string   `json:"snapshot_id,omitempty"`
	SnapshotHost  string   `json:"snapshot_host,omitempty"`
	Paths         []string `json:"paths,omitempty"`
	Error         string   `json:"error,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

func replaceBackupsTx(tx *sql.Tx, hostID int64, now string, backups []Backup) error {
	seen := make([]string, 0, len(backups))
	for _, b := range backups {
		name := strings.TrimSpace(b.Name)
		if name == "" {
			continue
		}
		seen = append(seen, name)
		paths, err := json.Marshal(b.Paths)
		if err != nil {
			return fmt.Errorf("marshal backup paths: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO backups (host_id, name, kind, status, container_name, last_success_at, last_checked_at, snapshot_id, snapshot_host, paths_json, error, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(host_id, name) DO UPDATE SET
				kind = excluded.kind,
				status = excluded.status,
				container_name = excluded.container_name,
				last_success_at = excluded.last_success_at,
				last_checked_at = excluded.last_checked_at,
				snapshot_id = excluded.snapshot_id,
				snapshot_host = excluded.snapshot_host,
				paths_json = excluded.paths_json,
				error = excluded.error,
				updated_at = excluded.updated_at
		`, hostID, name, b.Kind, b.Status, b.ContainerName, b.LastSuccessAt, b.LastCheckedAt, b.SnapshotID, b.SnapshotHost, string(paths), b.Error, now); err != nil {
			return fmt.Errorf("upsert backup: %w", err)
		}
	}

	if len(seen) == 0 {
		_, err := tx.Exec(`DELETE FROM backups WHERE host_id = ?`, hostID)
		return err
	}
	args := make([]any, 0, len(seen)+1)
	args = append(args, hostID)
	placeholders := make([]string, 0, len(seen))
	for _, name := range seen {
		placeholders = append(placeholders, "?")
		args = append(args, name)
	}
	_, err := tx.Exec(`DELETE FROM backups WHERE host_id = ? AND name NOT IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}

func (s *Store) backupsForHost(hostID int64) ([]Backup, error) {
	rows, err := s.DB.Query(`
		SELECT b.id, b.host_id, h.hostname, b.name, b.kind, b.status, b.container_name,
		       b.last_success_at, b.last_checked_at, b.snapshot_id, b.snapshot_host,
		       b.paths_json, b.error, b.updated_at
		FROM backups b
		INNER JOIN hosts h ON h.id = b.host_id
		WHERE b.host_id = ?
		ORDER BY b.name
	`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBackups(rows)
}

func (s *Store) AllBackups() ([]Backup, error) {
	rows, err := s.DB.Query(`
		SELECT b.id, b.host_id, h.hostname, b.name, b.kind, b.status, b.container_name,
		       b.last_success_at, b.last_checked_at, b.snapshot_id, b.snapshot_host,
		       b.paths_json, b.error, b.updated_at
		FROM backups b
		INNER JOIN hosts h ON h.id = b.host_id
		ORDER BY h.hostname, b.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBackups(rows)
}

func (s *Store) ListBackupsForHosts(hostIDs []int64) ([]Backup, error) {
	if len(hostIDs) == 0 {
		return []Backup{}, nil
	}
	args := make([]any, 0, len(hostIDs))
	placeholders := make([]string, 0, len(hostIDs))
	for _, id := range hostIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := s.DB.Query(`
		SELECT b.id, b.host_id, h.hostname, b.name, b.kind, b.status, b.container_name,
		       b.last_success_at, b.last_checked_at, b.snapshot_id, b.snapshot_host,
		       b.paths_json, b.error, b.updated_at
		FROM backups b
		INNER JOIN hosts h ON h.id = b.host_id
		WHERE b.host_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY h.hostname, b.name
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBackups(rows)
}

func scanBackups(rows *sql.Rows) ([]Backup, error) {
	out := []Backup{}
	for rows.Next() {
		var b Backup
		var pathsJSON string
		if err := rows.Scan(&b.ID, &b.HostID, &b.Hostname, &b.Name, &b.Kind, &b.Status, &b.ContainerName, &b.LastSuccessAt, &b.LastCheckedAt, &b.SnapshotID, &b.SnapshotHost, &pathsJSON, &b.Error, &b.UpdatedAt); err != nil {
			return nil, err
		}
		if pathsJSON != "" {
			_ = json.Unmarshal([]byte(pathsJSON), &b.Paths)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
