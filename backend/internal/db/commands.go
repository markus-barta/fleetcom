package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// MarkCommandResult records bosun's report back. status must be 'done',
// 'failed', or 'restarting'. The 'restarting' value (FLEET-85) is the
// agent.update signal — bosun has finished the destructive part of an
// update and is about to die; reconciliation happens later when the
// new bosun's first heartbeat arrives. 'done' / 'failed' are terminal.
//
// result is the handler's structured output (stdout tail, exit code,
// affected resource IDs, etc.), error is a short human message. Either
// may be empty.
func (s *Store) MarkCommandResult(id int64, host, status string, result, errStr string) error {
	if status != "done" && status != "failed" && status != "restarting" {
		return fmt.Errorf("invalid status: %s", status)
	}
	if status == "restarting" {
		// Only valid transition: executing → restarting. Stays
		// non-terminal (no completed_at, no result yet).
		res, err := s.DB.Exec(`
			UPDATE host_commands
			SET status = 'restarting', result = ?, error = ?
			WHERE id = ? AND host = ? AND status = 'executing'
		`, result, errStr, id, host)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("command %d not in executing state for host %s", id, host)
		}
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE host_commands
		SET status = ?, result = ?, error = ?, completed_at = ?
		WHERE id = ? AND host = ? AND status IN ('executing','restarting')
	`, status, result, errStr, now, id, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("command %d not in executing or restarting state for host %s", id, host)
	}
	return nil
}

// CancelCommand stops a command. Works for pending (pre-pickup, no
// side effects on host) AND executing (post-pickup — admin knows the
// host won't come back, or bosun is dead, or they just want to give
// up waiting). For executing, cancellation races cleanly with a late
// MarkCommandResult: that call has `status = 'executing'` in its
// WHERE clause and will no-op if we flipped to 'cancelled' first.
func (s *Store) CancelCommand(id int64) error {
	res, err := s.DB.Exec(`
		UPDATE host_commands
		SET status = 'cancelled', completed_at = datetime('now'),
		    error = CASE WHEN status = 'executing' THEN 'cancelled by admin while in flight' ELSE error END
		WHERE id = ? AND status IN ('pending','executing')
	`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("command %d is not pending or executing", id)
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

// ExpireStuckCommands moves commands stuck in non-terminal states past
// a threshold (typically a few multiples of the heartbeat interval) to
// 'failed' so the UI doesn't show them as forever-in-progress when a
// host went dark mid-execution. 'restarting' agent.update commands
// (FLEET-85) are also covered — if the new bosun never reports back
// with the new agent_version, the update is considered failed.
//
// Retries transient SQLITE_BUSY (the 5s busy_timeout on open isn't
// always enough when a heartbeat transaction spans the same rows)
// before giving up.
func (s *Store) ExpireStuckCommands(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		res, err := s.DB.Exec(`
			UPDATE host_commands
			SET status = 'failed',
			    error = CASE
			        WHEN status = 'restarting' THEN 'timeout: bosun did not return on the new version within window'
			        ELSE 'timeout: bosun did not report within window'
			    END,
			    completed_at = datetime('now')
			WHERE status IN ('executing','restarting') AND picked_at < ?
		`, cutoff)
		if err == nil {
			n, _ := res.RowsAffected()
			return n, nil
		}
		lastErr = err
		msg := err.Error()
		if !strings.Contains(msg, "database is locked") && !strings.Contains(msg, "SQLITE_BUSY") {
			return 0, err
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return 0, lastErr
}

// PendingOrInflightAgentUpdate returns the id of any agent.update
// command for the host that is still in pending / executing /
// restarting state, or 0 if none. Used by the EnqueueCommand handler
// to enforce idempotency (FLEET-85): only one update may be in flight
// for a given host at a time.
func (s *Store) PendingOrInflightAgentUpdate(host string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`
		SELECT id FROM host_commands
		WHERE host = ? AND kind = 'agent.update' AND status IN ('pending','executing','restarting')
		ORDER BY id DESC LIMIT 1
	`, host).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// DeploymentShapeForHost returns the host's reported deployment_shape.
// Returns "" if the host doesn't exist or hasn't been bosun-classified
// yet. Used by the agent.update pre-flight (FLEET-85).
func (s *Store) DeploymentShapeForHost(host string) (string, error) {
	var shape string
	err := s.DB.QueryRow(`SELECT deployment_shape FROM hosts WHERE hostname = ?`, host).Scan(&shape)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return shape, err
}

// AgentVersionForHost returns the host's last-reported agent_version.
// Used by the agent.update pre-flight to capture the pre-update value
// so the post-restart heartbeat can be reconciled (FLEET-85).
func (s *Store) AgentVersionForHost(host string) (string, error) {
	var v string
	err := s.DB.QueryRow(`SELECT agent_version FROM hosts WHERE hostname = ?`, host).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// BootIDForHost returns the host's last-reported boot_id (the contents of
// /proc/sys/kernel/random/boot_id at the time of the last heartbeat). Used
// by the host.reboot pre-flight (FLEET-369.1) to capture the value so the
// post-reboot heartbeat can be reconciled — a different boot_id on return
// is the canonical "the kernel actually rebooted" signal.
func (s *Store) BootIDForHost(host string) (string, error) {
	var v string
	err := s.DB.QueryRow(`SELECT boot_id FROM hosts WHERE hostname = ?`, host).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// AllowRebootForHost returns the host-level kill switch for host.reboot.
// Defaults true on insert; admin can flip via SetAllowReboot. Used by the
// host.reboot pre-flight so an operator can disable reboots on a single
// host without touching the global FLEETCOM_DESTRUCTIVE_COMMANDS flag.
func (s *Store) AllowRebootForHost(host string) (bool, error) {
	var v int
	err := s.DB.QueryRow(`SELECT allow_reboot FROM hosts WHERE hostname = ?`, host).Scan(&v)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return v != 0, err
}

// SetAllowReboot flips the per-host reboot kill switch. Returns an error
// if the host doesn't exist (so callers can return 404 cleanly).
func (s *Store) SetAllowReboot(host string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.DB.Exec(`UPDATE hosts SET allow_reboot = ? WHERE hostname = ?`, v, host)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("host not found: %s", host)
	}
	return nil
}

// PendingOrInflightHostReboot returns the id of any host.reboot command
// for the host that is still pending / executing / restarting, or 0 if
// none. Used by EnqueueCommand to enforce idempotency: only one reboot
// may be in flight for a given host at a time.
func (s *Store) PendingOrInflightHostReboot(host string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`
		SELECT id FROM host_commands
		WHERE host = ? AND kind = 'host.reboot' AND status IN ('pending','executing','restarting')
		ORDER BY id DESC LIMIT 1
	`, host).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// ReconcileHostReboot is called from the heartbeat handler. If the host
// has a host.reboot command in 'restarting' state and the just-arrived
// heartbeat reports a non-empty boot_id that differs from the one
// captured at enqueue time, mark the command done — the kernel actually
// rebooted. Returns the count of commands reconciled.
//
// Stuck "restarting" reboots are swept by ExpireStuckCommands the same
// way agent.update is.
func (s *Store) ReconcileHostReboot(host, currentBootID string) (int, error) {
	if currentBootID == "" {
		return 0, nil
	}
	rows, err := s.DB.Query(`
		SELECT id, params FROM host_commands
		WHERE host = ? AND kind = 'host.reboot' AND status = 'restarting'
	`, host)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type pending struct {
		id     int64
		params string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.params); err != nil {
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	for _, p := range todo {
		var ps struct {
			PreRebootBootID string `json:"pre_reboot_boot_id"`
		}
		_ = json.Unmarshal([]byte(p.params), &ps)
		if ps.PreRebootBootID != "" && currentBootID != ps.PreRebootBootID {
			result := fmt.Sprintf(`{"reconciled_via_heartbeat":true,"pre_reboot_boot_id":%q,"now_boot_id":%q}`, ps.PreRebootBootID, currentBootID)
			if _, err := s.DB.Exec(`
				UPDATE host_commands
				SET status='done', result=?, completed_at=?
				WHERE id=? AND status='restarting'
			`, result, now, p.id); err == nil {
				count++
			}
		}
	}
	return count, nil
}

// ReconcileAgentUpdate is called from the heartbeat handler. If the
// host has any agent.update commands in 'restarting' state and the
// just-arrived heartbeat reports a non-empty agent_version that
// differs from the version captured at enqueue time, mark them done
// — bosun came back on the new version. Returns the count of
// commands reconciled.
//
// If a "restarting" command is older than the timeout window the
// generic ExpireStuckCommands sweeper will mark it failed instead.
func (s *Store) ReconcileAgentUpdate(host, currentAgentVersion string) (int, error) {
	if currentAgentVersion == "" {
		return 0, nil
	}
	rows, err := s.DB.Query(`
		SELECT id, params FROM host_commands
		WHERE host = ? AND kind = 'agent.update' AND status = 'restarting'
	`, host)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type pending struct {
		id     int64
		params string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.params); err != nil {
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	for _, p := range todo {
		var ps struct {
			Target            string `json:"target"`
			PreUpdateVersion  string `json:"pre_update_version"`
			PreUpdateAgentVer string `json:"pre_update_agent_version"`
		}
		_ = json.Unmarshal([]byte(p.params), &ps)
		pre := ps.PreUpdateVersion
		if pre == "" {
			pre = ps.PreUpdateAgentVer
		}
		// If the bosun is reporting a version that differs from the
		// version it had pre-update, the swap took. We don't enforce
		// exact target match — "latest" requests can resolve to any
		// newer build, and tag-based requests may resolve to a
		// formatted-version string that doesn't equal the requested
		// tag verbatim. The dashboard displays both for the operator.
		if pre != "" && currentAgentVersion != pre {
			result := fmt.Sprintf(`{"reconciled_via_heartbeat":true,"pre_update":%q,"now":%q}`, pre, currentAgentVersion)
			if _, err := s.DB.Exec(`
				UPDATE host_commands
				SET status='done', result=?, completed_at=?
				WHERE id=? AND status='restarting'
			`, result, now, p.id); err == nil {
				count++
			}
		}
	}
	return count, nil
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
