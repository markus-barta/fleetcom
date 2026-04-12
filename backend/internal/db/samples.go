package db

import (
	"database/sql"
	"fmt"
	"time"
)

// NormalizeContainerStatus mirrors the frontend containerStatus() mapping.
func NormalizeContainerStatus(state string) string {
	switch state {
	case "running":
		return "ok"
	case "paused":
		return "warn"
	case "exited", "dead":
		return "crit"
	default:
		return "dead"
	}
}

// NormalizeAgentStatus mirrors the frontend agentStatus() mapping.
func NormalizeAgentStatus(status string) string {
	switch status {
	case "online":
		return "ok"
	case "degraded":
		return "warn"
	case "offline":
		return "crit"
	default:
		return "dead"
	}
}

// recordSamples inserts one status_samples row per entity being reported in
// a heartbeat. The host itself is always "ok" at heartbeat time (the fact
// that a heartbeat arrived implies the host is alive). Must run inside the
// heartbeat transaction so the samples are consistent with current state.
func recordSamples(tx *sql.Tx, ts, hostname string, containers []Container, agents []Agent) error {
	stmt, err := tx.Prepare(`INSERT INTO status_samples (entity_type, entity_key, ts, status) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare sample insert: %w", err)
	}
	defer stmt.Close()

	if _, err := stmt.Exec("host", hostname, ts, "ok"); err != nil {
		return fmt.Errorf("insert host sample: %w", err)
	}

	for _, c := range containers {
		key := hostname + "/" + c.Name
		if _, err := stmt.Exec("container", key, ts, NormalizeContainerStatus(c.State)); err != nil {
			return fmt.Errorf("insert container sample: %w", err)
		}
	}

	for _, a := range agents {
		key := hostname + "/" + a.Name
		if _, err := stmt.Exec("agent", key, ts, NormalizeAgentStatus(a.Status)); err != nil {
			return fmt.Errorf("insert agent sample: %w", err)
		}
	}

	return nil
}

// Bucket represents one cell of a history strip. Status is the worst status
// observed in the bucket ("ok" / "warn" / "crit"), or "none" if no samples
// were recorded (which typically means an outage for the entity in question).
type Bucket struct {
	Index  int    `json:"i"`
	Status string `json:"s"`
	OKPct  int    `json:"p"`
	Count  int    `json:"n"`
}

// ScaleSpec defines a named time window for history strips. Each scale renders
// Buckets cells spanning Window, so BucketDuration = Window / Buckets.
type ScaleSpec struct {
	Name    string
	Window  time.Duration
	Buckets int
}

// Scales is the canonical list served by the /api/history endpoint.
// 60 buckets per scale keeps rendering uniform across all strips.
var Scales = []ScaleSpec{
	{Name: "1min", Window: time.Minute, Buckets: 60},
	{Name: "15min", Window: 15 * time.Minute, Buckets: 60},
	{Name: "1h", Window: time.Hour, Buckets: 60},
	{Name: "1d", Window: 24 * time.Hour, Buckets: 60},
	{Name: "14d", Window: 14 * 24 * time.Hour, Buckets: 60},
	{Name: "1MO", Window: 30 * 24 * time.Hour, Buckets: 60},
	{Name: "1Y", Window: 365 * 24 * time.Hour, Buckets: 60},
}

// FindScale returns the scale spec by name, or zero value + false.
func FindScale(name string) (ScaleSpec, bool) {
	for _, s := range Scales {
		if s.Name == name {
			return s, true
		}
	}
	return ScaleSpec{}, false
}

// HistoryBuckets returns Buckets cells for the given entity over the given
// scale, ordered oldest-first. Gaps (buckets without samples) are returned
// as Status="none", letting the frontend render them as outages.
func (s *Store) HistoryBuckets(entityType, entityKey string, scale ScaleSpec) ([]Bucket, error) {
	end := time.Now().UTC()
	start := end.Add(-scale.Window)
	bucketSec := int64(scale.Window.Seconds()) / int64(scale.Buckets)
	if bucketSec < 1 {
		bucketSec = 1
	}
	// Snap start to a bucket-aligned boundary so that, for the 1h scale
	// (bucketSec=60), bucket edges fall on minute boundaries — matching
	// cron's firing times. Without this, bucket edges drift relative to
	// heartbeat arrival and create periodic aliasing gaps.
	startEpoch := start.Unix()
	startEpoch -= startEpoch % bucketSec

	rows, err := s.DB.Query(`
		SELECT
			CAST((CAST(strftime('%s', ts) AS INTEGER) - ?) / ? AS INTEGER) AS bucket,
			SUM(CASE WHEN status = 'ok'   THEN 1 ELSE 0 END) AS ok_count,
			SUM(CASE WHEN status = 'warn' THEN 1 ELSE 0 END) AS warn_count,
			SUM(CASE WHEN status = 'crit' THEN 1 ELSE 0 END) AS crit_count,
			SUM(CASE WHEN status = 'dead' THEN 1 ELSE 0 END) AS dead_count,
			COUNT(*) AS total
		FROM status_samples
		WHERE entity_type = ? AND entity_key = ? AND ts >= ?
		GROUP BY bucket
		ORDER BY bucket
	`, startEpoch, bucketSec, entityType, entityKey, time.Unix(startEpoch, 0).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("query buckets: %w", err)
	}
	defer rows.Close()

	out := make([]Bucket, scale.Buckets)
	for i := range out {
		out[i] = Bucket{Index: i, Status: "none"}
	}

	for rows.Next() {
		var bucket, okC, warnC, critC, deadC, total int
		if err := rows.Scan(&bucket, &okC, &warnC, &critC, &deadC, &total); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}
		if bucket < 0 || bucket >= scale.Buckets || total == 0 {
			continue
		}
		// Worst status wins: crit > warn > dead > ok. Dead ranks between warn
		// and ok because it means the entity was reported as unknown/missing
		// but *was* reported — contrast that with "none" which means no
		// sample at all.
		status := "ok"
		switch {
		case critC > 0:
			status = "crit"
		case warnC > 0:
			status = "warn"
		case deadC > 0:
			status = "dead"
		}
		okPct := 0
		if total > 0 {
			okPct = (okC * 100) / total
		}
		out[bucket] = Bucket{Index: bucket, Status: status, OKPct: okPct, Count: total}
	}

	return out, rows.Err()
}

// EntityFirstSample returns the timestamp of the oldest sample for an entity,
// or "" if none exist. Lets the client derive per-scale coverage from one
// fetch instead of probing every scale.
func (s *Store) EntityFirstSample(entityType, entityKey string) (string, error) {
	var ts sql.NullString
	err := s.DB.QueryRow(
		`SELECT MIN(ts) FROM status_samples WHERE entity_type = ? AND entity_key = ?`,
		entityType, entityKey,
	).Scan(&ts)
	if err != nil {
		return "", fmt.Errorf("query first sample: %w", err)
	}
	if !ts.Valid {
		return "", nil
	}
	return ts.String, nil
}

// PurgeOldSamples deletes samples older than the retention window. Runs on
// startup and periodically via a background goroutine.
func (s *Store) PurgeOldSamples(retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	res, err := s.DB.Exec(`DELETE FROM status_samples WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge samples: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
