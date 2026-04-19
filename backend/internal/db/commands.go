package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// HostCommand represents one unit of work enqueued for a host's bosun.
// Lifecycle: pending → executing (when bosun picks it up) → done|failed
// (when bosun POSTs the result). cancelled is a pre-pickup drop by admin.
type HostCommand struct {
	ID             int64           `json:"id"`
	Host           string          `json:"host"`
	Kind           string          `json:"kind"`
	Params         json.RawMessage `json:"params"`
	Status         string          `json:"status"`
	IssuedByUserID *int64          `json:"issued_by_user_id,omitempty"`
	IssuedAt       string          `json:"issued_at"`
	PickedAt       string          `json:"picked_at,omitempty"`
	CompletedAt    string          `json:"completed_at,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// EnqueueCommand inserts a new pending command for a host. Caller is
// responsible for checking admin rights on the command kind.
func (s *Store) EnqueueCommand(host, kind string, params interface{}, userID *int64) (int64, error) {
	p := []byte("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return 0, fmt.Errorf("marshal params: %w", err)
		}
		p = b
	}
	var id int64
	err := s.DB.QueryRow(`
		INSERT INTO host_commands (host, kind, params, status, issued_by_user_id)
		VALUES (?, ?, ?, 'pending', ?)
		RETURNING id
	`, host, kind, string(p), userID).Scan(&id)
	return id, err
}

// PendingCommandsForHost returns commands in status='pending' for one
// host AND flips them to 'executing' in the same transaction so bosun
// sees each command exactly once across heartbeats. The `picked_at`
// timestamp anchors the execution window for later timeout detection.
func (s *Store) PendingCommandsForHost(host string) ([]HostCommand, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.Query(`
		SELECT id, host, kind, params, status, issued_by_user_id, issued_at, picked_at, completed_at, result, error
		FROM host_commands
		WHERE host = ? AND status = 'pending'
		ORDER BY id ASC
	`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostCommand
	var ids []int64
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE host_commands SET status = 'executing', picked_at = ? WHERE id = ?`, now, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Return them with the updated status so bosun sees the consistent view.
	for i := range out {
		out[i].Status = "executing"
		out[i].PickedAt = now
	}
	return out, nil
}

// MarkCommandResult records bosun's report back. status must be 'done'
// or 'failed'. result is the handler's structured output (stdout tail,
// exit code, affected resource IDs, etc.), error is a short human
// message. Either may be empty.
func (s *Store) MarkCommandResult(id int64, host, status string, result, errStr string) error {
	if status != "done" && status != "failed" {
		return fmt.Errorf("invalid status: %s", status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE host_commands
		SET status = ?, result = ?, error = ?, completed_at = ?
		WHERE id = ? AND host = ? AND status = 'executing'
	`, status, result, errStr, now, id, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("command %d not in executing state for host %s", id, host)
	}
	return nil
}

// CancelCommand drops a pending command (pre-pickup only — executing
// commands are in flight and can't be un-issued).
func (s *Store) CancelCommand(id int64) error {
	res, err := s.DB.Exec(`UPDATE host_commands SET status = 'cancelled', completed_at = datetime('now') WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("command %d not pending", id)
	}
	return nil
}

// CommandsForHost returns the most-recent N commands for a host,
// newest first. Powers the per-host audit history in the UI.
func (s *Store) CommandsForHost(host string, limit int) ([]HostCommand, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.DB.Query(`
		SELECT id, host, kind, params, status, issued_by_user_id, issued_at, picked_at, completed_at, result, error
		FROM host_commands
		WHERE host = ?
		ORDER BY id DESC
		LIMIT ?
	`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostCommand{}
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExpireStuckCommands moves commands stuck in 'executing' past a
// threshold (typically a few multiples of the heartbeat interval) to
// 'failed' so the UI doesn't show them as forever-in-progress when a
// host went dark mid-execution.
func (s *Store) ExpireStuckCommands(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE host_commands
		SET status = 'failed', error = 'timeout: bosun did not report within window', completed_at = datetime('now')
		WHERE status = 'executing' AND picked_at < ?
	`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanCommand(rows *sql.Rows) (HostCommand, error) {
	var c HostCommand
	var params, result sql.NullString
	var userID sql.NullInt64
	var picked, completed, errStr sql.NullString
	if err := rows.Scan(&c.ID, &c.Host, &c.Kind, &params, &c.Status, &userID, &c.IssuedAt, &picked, &completed, &result, &errStr); err != nil {
		return c, err
	}
	if params.Valid {
		c.Params = json.RawMessage(params.String)
	}
	if result.Valid {
		c.Result = json.RawMessage(result.String)
	}
	if userID.Valid {
		c.IssuedByUserID = &userID.Int64
	}
	c.PickedAt = picked.String
	c.CompletedAt = completed.String
	c.Error = errStr.String
	return c, nil
}
