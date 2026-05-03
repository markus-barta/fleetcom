package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// FLEET-108: operator activity log. Rows are written by busy() in the
// browser via POST /api/activity. The store layer is intentionally thin
// — all the policy (auth, scoping, retention) lives in the API layer
// and the cleanup loop.

type ActivityEvent struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	UserID     int64  `json:"user_id"`
	UserEmail  string `json:"user_email,omitempty"` // joined from users on read
	Verb       string `json:"verb"`
	TargetType string `json:"target_type,omitempty"`
	TargetKey  string `json:"target_key,omitempty"`
	Outcome    string `json:"outcome"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

// RecordActivity writes one row. Called by the POST /api/activity handler
// after the operator's action settles. user_id=0 is allowed (system-initiated)
// but the standard call comes with the session user.
func (s *Store) RecordActivity(e ActivityEvent) (int64, error) {
	res, err := s.DB.Exec(
		`INSERT INTO activity_events (user_id, verb, target_type, target_key, outcome, duration_ms, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.UserID, e.Verb, e.TargetType, e.TargetKey, e.Outcome, e.DurationMs, e.Error,
	)
	if err != nil {
		return 0, fmt.Errorf("insert activity: %w", err)
	}
	return res.LastInsertId()
}

// ListActivityOpts narrows the result set. All fields are optional.
// Limit defaults to 200 if zero; capped at 1000.
type ListActivityOpts struct {
	UserID  int64  // when > 0: only this user's rows. When 0: all rows (admin).
	Since   string // RFC3339; only rows with ts >= Since
	Verb    string // exact match (e.g. "REVOKE")
	Target  string // partial match on target_type:target_key
	Outcome string // exact match: "ok" | "err" | "pend"
	Limit   int
}

func (s *Store) ListActivity(opts ListActivityOpts) ([]ActivityEvent, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}

	where := []string{"1=1"}
	args := []interface{}{}
	if opts.UserID > 0 {
		where = append(where, "a.user_id = ?")
		args = append(args, opts.UserID)
	}
	if opts.Since != "" {
		where = append(where, "a.ts >= ?")
		args = append(args, opts.Since)
	}
	if opts.Verb != "" {
		where = append(where, "a.verb = ?")
		args = append(args, opts.Verb)
	}
	if opts.Outcome != "" {
		where = append(where, "a.outcome = ?")
		args = append(args, opts.Outcome)
	}
	if opts.Target != "" {
		where = append(where, "(a.target_type LIKE ? OR a.target_key LIKE ?)")
		pat := "%" + opts.Target + "%"
		args = append(args, pat, pat)
	}

	q := `SELECT a.id, a.ts, a.user_id, COALESCE(u.email, ''), a.verb, a.target_type, a.target_key, a.outcome, a.duration_ms, a.error
	      FROM activity_events a
	      LEFT JOIN users u ON u.id = a.user_id
	      WHERE ` + strings.Join(where, " AND ") + `
	      ORDER BY a.ts DESC
	      LIMIT ?`
	args = append(args, limit)

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()

	var out []ActivityEvent
	for rows.Next() {
		var e ActivityEvent
		if err := rows.Scan(&e.ID, &e.TS, &e.UserID, &e.UserEmail,
			&e.Verb, &e.TargetType, &e.TargetKey, &e.Outcome, &e.DurationMs, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneOldActivity drops rows older than the retention window. Returns
// the number of deleted rows. Hooked into the existing 6h cleanup loop
// in cmd/server/main.go.
//
// Default policy: keep all rows for 7 days, plus retain CREATE / DELETE /
// GRANT / REVOKE for 30 days because those are the rows operators look
// back at when investigating "who changed what last quarter".
func (s *Store) PruneOldActivity(shortRetention, longRetention time.Duration) (int64, error) {
	shortCutoff := time.Now().UTC().Add(-shortRetention).Format(time.RFC3339)
	longCutoff := time.Now().UTC().Add(-longRetention).Format(time.RFC3339)
	res, err := s.DB.Exec(
		`DELETE FROM activity_events
		 WHERE ts < ?
		   AND (verb NOT IN ('CREATE','DELETE','GRANT','REVOKE') OR ts < ?)`,
		shortCutoff, longCutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("prune activity: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// activitySinceParam validates and normalizes the optional `since`
// query param. Returns ("" , nil) if missing.
func ActivityValidateSince(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339), nil
}

// _ keeps sql import used even if no callsites of the helper above survive a refactor.
var _ = sql.ErrNoRows
