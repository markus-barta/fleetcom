package db

import (
	"database/sql"
	"fmt"
	"time"
)

// RequestUpdateByHostname sets update_requested_at = now on a single host.
// A follow-up heartbeat will consume the flag and send the update command.
// Returns true if the host existed and was flagged.
func (s *Store) RequestUpdateByHostname(hostname string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`UPDATE hosts SET update_requested_at = ? WHERE hostname = ?`, now, hostname)
	if err != nil {
		return false, fmt.Errorf("flag host: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RequestUpdateAll flags every host the user can access. For admins,
// pass 0 as userID to touch every host.
func (s *Store) RequestUpdateAll(userID int64, isAdmin bool) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var res sql.Result
	var err error
	if isAdmin {
		res, err = s.DB.Exec(`UPDATE hosts SET update_requested_at = ?`, now)
	} else {
		res, err = s.DB.Exec(
			`UPDATE hosts SET update_requested_at = ?
			 WHERE id IN (SELECT host_id FROM user_host_access WHERE user_id = ?)`,
			now, userID,
		)
	}
	if err != nil {
		return 0, fmt.Errorf("flag all hosts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// consumePendingCommand atomically reads and clears update_requested_at
// for a host inside a transaction. Returns true if an update was pending.
// Must run inside the heartbeat transaction so the flag isn't lost.
func consumePendingCommand(tx *sql.Tx, hostID int64) (bool, error) {
	var flag string
	err := tx.QueryRow(`SELECT update_requested_at FROM hosts WHERE id = ?`, hostID).Scan(&flag)
	if err != nil {
		return false, fmt.Errorf("read update_requested_at: %w", err)
	}
	if flag == "" {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE hosts SET update_requested_at = '' WHERE id = ?`, hostID); err != nil {
		return false, fmt.Errorf("clear update_requested_at: %w", err)
	}
	return true, nil
}
