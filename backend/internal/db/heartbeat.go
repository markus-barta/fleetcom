package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type Host struct {
	ID                int64       `json:"id"`
	Hostname          string      `json:"hostname"`
	OS                string      `json:"os"`
	Kernel            string      `json:"kernel"`
	UptimeSeconds     int64       `json:"uptime_seconds"`
	AgentVersion      string      `json:"agent_version"`
	LastSeen          string      `json:"last_seen"`
	CreatedAt         string      `json:"created_at,omitempty"`
	UpdateRequestedAt string      `json:"update_requested_at,omitempty"`
	HwLive            *HwLive     `json:"hw_live,omitempty"`
	HwLiveAt          string      `json:"hw_live_at,omitempty"`
	Containers        []Container `json:"containers"`
	Agents            []Agent     `json:"agents"`
}

// HardwareHeartbeat carries optional hardware/metadata fields from a heartbeat.
// All three sub-fields are independently optional: bosun omits Static on beats
// where it hasn't changed, Live is present on every beat once collection is
// active, and Fastfetch is only included on refresh (startup + daily).
type HardwareHeartbeat struct {
	Static    *HwStatic
	Live      *HwLive
	Fastfetch json.RawMessage
}

type Container struct {
	ID           int64  `json:"id"`
	HostID       int64  `json:"host_id"`
	Name         string `json:"name"`
	Image        string `json:"image"`
	State        string `json:"state"`
	Health       string `json:"health"`
	RestartCount int    `json:"restart_count"`
	StartedAt    string `json:"started_at"`
	ExitCode     int    `json:"exit_code"`
	OOMKilled    bool   `json:"oom_killed"`
	CrashLoop    bool   `json:"crash_loop"`
	LastSeen     string `json:"last_seen"`
}

type Agent struct {
	ID        int64  `json:"id"`
	HostID    int64  `json:"host_id"`
	Name      string `json:"name"`
	AgentType string `json:"agent_type"`
	Status    string `json:"status"`
	LastSeen  string `json:"last_seen"`
}

// UpsertHeartbeat persists a heartbeat and, atomically in the same tx,
// consumes a pending "update" command if one was flagged for this host.
// Returns a non-empty command string when the caller should relay a
// command to the agent in the heartbeat response.
func (s *Store) UpsertHeartbeat(hostname, os, kernel string, uptimeSeconds int64, agentVersion string, containers []Container, agents []Agent, hw *HardwareHeartbeat) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.DB.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Upsert host
	var hostID int64
	err = tx.QueryRow(`
		INSERT INTO hosts (hostname, os, kernel, uptime_seconds, agent_version, last_seen)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(hostname) DO UPDATE SET
			os = excluded.os,
			kernel = excluded.kernel,
			uptime_seconds = excluded.uptime_seconds,
			agent_version = excluded.agent_version,
			last_seen = excluded.last_seen
		RETURNING id
	`, hostname, os, kernel, uptimeSeconds, agentVersion, now).Scan(&hostID)
	if err != nil {
		return "", fmt.Errorf("upsert host: %w", err)
	}

	// Persist hardware fields (all optional).
	if hw != nil {
		if hw.Static != nil {
			blob, err := json.Marshal(hw.Static)
			if err != nil {
				return "", fmt.Errorf("marshal hw_static: %w", err)
			}
			if _, err := tx.Exec(`UPDATE hosts SET hw_static = ? WHERE id = ?`, string(blob), hostID); err != nil {
				return "", fmt.Errorf("update hw_static: %w", err)
			}
		}
		if hw.Live != nil {
			blob, err := json.Marshal(hw.Live)
			if err != nil {
				return "", fmt.Errorf("marshal hw_live: %w", err)
			}
			if _, err := tx.Exec(`UPDATE hosts SET hw_live = ?, hw_live_at = ? WHERE id = ?`, string(blob), now, hostID); err != nil {
				return "", fmt.Errorf("update hw_live: %w", err)
			}
			if err := insertHostMetric(tx, hostID, now, hw.Live); err != nil {
				return "", err
			}
		}
		if len(hw.Fastfetch) > 0 {
			if _, err := tx.Exec(`UPDATE hosts SET fastfetch_json = ?, fastfetch_at = ? WHERE id = ?`, string(hw.Fastfetch), now, hostID); err != nil {
				return "", fmt.Errorf("update fastfetch: %w", err)
			}
		}
	}

	// Detect crash loops: read previous restart counts before replacing
	prevCounts := map[string]int{}
	{
		rows, err := tx.Query(`SELECT name, restart_count FROM containers WHERE host_id = ?`, hostID)
		if err == nil {
			for rows.Next() {
				var n string
				var rc int
				rows.Scan(&n, &rc)
				prevCounts[n] = rc
			}
			rows.Close()
		}
	}

	// Replace containers: delete old, insert new
	if _, err := tx.Exec(`DELETE FROM containers WHERE host_id = ?`, hostID); err != nil {
		return "", fmt.Errorf("delete containers: %w", err)
	}
	for i, c := range containers {
		oom := 0
		if c.OOMKilled {
			oom = 1
		}
		if _, err := tx.Exec(`INSERT INTO containers (host_id, name, image, state, health, restart_count, started_at, exit_code, oom_killed, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			hostID, c.Name, c.Image, c.State, c.Health, c.RestartCount, c.StartedAt, c.ExitCode, oom, now); err != nil {
			return "", fmt.Errorf("insert container: %w", err)
		}
		// Record restart events if restart count increased
		if prev, ok := prevCounts[c.Name]; ok && c.RestartCount > prev {
			delta := c.RestartCount - prev
			for range delta {
				tx.Exec(`INSERT INTO container_events (host_id, container_name, event_type, exit_code, oom_killed, ts) VALUES (?, ?, 'restart', ?, ?, ?)`,
					hostID, c.Name, c.ExitCode, oom, now)
			}
		}
		// Detect crash loop: ≥3 restarts in last 5 minutes
		var recentRestarts int
		cutoff := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
		tx.QueryRow(`SELECT COUNT(*) FROM container_events WHERE host_id = ? AND container_name = ? AND event_type = 'restart' AND ts >= ?`,
			hostID, c.Name, cutoff).Scan(&recentRestarts)
		containers[i].CrashLoop = recentRestarts >= 3
	}

	// Replace agents: delete old, insert new
	if _, err := tx.Exec(`DELETE FROM agents WHERE host_id = ?`, hostID); err != nil {
		return "", fmt.Errorf("delete agents: %w", err)
	}
	for _, a := range agents {
		if _, err := tx.Exec(`INSERT INTO agents (host_id, name, agent_type, status, last_seen) VALUES (?, ?, ?, ?, ?)`,
			hostID, a.Name, a.AgentType, a.Status, now); err != nil {
			return "", fmt.Errorf("insert agent: %w", err)
		}
	}

	if err := recordSamples(tx, now, hostname, containers, agents); err != nil {
		return "", err
	}

	// Consume any pending server-triggered command atomically with the
	// heartbeat so we never double-dispatch or lose a request on crash.
	command := ""
	if pending, err := consumePendingCommand(tx, hostID); err != nil {
		return "", err
	} else if pending {
		command = "update"
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return command, nil
}

func scanHosts(rows *sql.Rows) ([]Host, error) {
	var hosts []Host
	for rows.Next() {
		var h Host
		var liveBlob string
		if err := rows.Scan(&h.ID, &h.Hostname, &h.OS, &h.Kernel, &h.UptimeSeconds, &h.AgentVersion, &h.LastSeen, &liveBlob, &h.HwLiveAt, &h.CreatedAt, &h.UpdateRequestedAt); err != nil {
			return nil, err
		}
		if liveBlob != "" {
			var hl HwLive
			if err := json.Unmarshal([]byte(liveBlob), &hl); err == nil {
				h.HwLive = &hl
			}
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (s *Store) AllHosts() ([]Host, error) {
	rows, err := s.DB.Query(`SELECT id, hostname, os, kernel, uptime_seconds, agent_version, last_seen, hw_live, hw_live_at, created_at, update_requested_at FROM hosts ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts, err := scanHosts(rows)
	if err != nil {
		return nil, err
	}

	for i := range hosts {
		hosts[i].Containers, err = s.containersForHost(hosts[i].ID)
		if err != nil {
			return nil, err
		}
		hosts[i].Agents, err = s.agentsForHost(hosts[i].ID)
		if err != nil {
			return nil, err
		}
	}

	return hosts, nil
}

// HostsForUser returns hosts filtered by user_host_access for regular users.
func (s *Store) HostsForUser(userID int64) ([]Host, error) {
	rows, err := s.DB.Query(
		`SELECT h.id, h.hostname, h.os, h.kernel, h.uptime_seconds, h.agent_version, h.last_seen, h.hw_live, h.hw_live_at, h.created_at, h.update_requested_at
		 FROM hosts h
		 JOIN user_host_access uha ON h.id = uha.host_id
		 WHERE uha.user_id = ?
		 ORDER BY h.hostname`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts, err := scanHosts(rows)
	if err != nil {
		return nil, err
	}

	for i := range hosts {
		hosts[i].Containers, err = s.containersForHost(hosts[i].ID)
		if err != nil {
			return nil, err
		}
		hosts[i].Agents, err = s.agentsForHost(hosts[i].ID)
		if err != nil {
			return nil, err
		}
	}

	return hosts, nil
}

func (s *Store) containersForHost(hostID int64) ([]Container, error) {
	rows, err := s.DB.Query(`SELECT id, host_id, name, image, state, health, restart_count, started_at, exit_code, oom_killed, last_seen FROM containers WHERE host_id = ? ORDER BY name`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cutoff := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	var cs []Container
	for rows.Next() {
		var c Container
		var oom int
		if err := rows.Scan(&c.ID, &c.HostID, &c.Name, &c.Image, &c.State, &c.Health, &c.RestartCount, &c.StartedAt, &c.ExitCode, &oom, &c.LastSeen); err != nil {
			return nil, err
		}
		c.OOMKilled = oom != 0
		// Check crash loop from events table
		var recentRestarts int
		s.DB.QueryRow(`SELECT COUNT(*) FROM container_events WHERE host_id = ? AND container_name = ? AND event_type = 'restart' AND ts >= ?`,
			hostID, c.Name, cutoff).Scan(&recentRestarts)
		c.CrashLoop = recentRestarts >= 3
		cs = append(cs, c)
	}
	return cs, nil
}

func (s *Store) agentsForHost(hostID int64) ([]Agent, error) {
	rows, err := s.DB.Query(`SELECT id, host_id, name, agent_type, status, last_seen FROM agents WHERE host_id = ? ORDER BY name`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var as []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.HostID, &a.Name, &a.AgentType, &a.Status, &a.LastSeen); err != nil {
			return nil, err
		}
		as = append(as, a)
	}
	return as, nil
}
