package db

import (
	"fmt"
	"time"
)

type Host struct {
	ID            int64       `json:"id"`
	Hostname      string      `json:"hostname"`
	OS            string      `json:"os"`
	Kernel        string      `json:"kernel"`
	UptimeSeconds int64       `json:"uptime_seconds"`
	LastSeen      string      `json:"last_seen"`
	Containers    []Container `json:"containers"`
	Agents        []Agent     `json:"agents"`
}

type Container struct {
	ID       int64  `json:"id"`
	HostID   int64  `json:"host_id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    string `json:"state"`
	LastSeen string `json:"last_seen"`
}

type Agent struct {
	ID        int64  `json:"id"`
	HostID    int64  `json:"host_id"`
	Name      string `json:"name"`
	AgentType string `json:"agent_type"`
	Status    string `json:"status"`
	LastSeen  string `json:"last_seen"`
}

func (s *Store) UpsertHeartbeat(hostname, os, kernel string, uptimeSeconds int64, containers []Container, agents []Agent) error {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Upsert host
	var hostID int64
	err = tx.QueryRow(`
		INSERT INTO hosts (hostname, os, kernel, uptime_seconds, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(hostname) DO UPDATE SET
			os = excluded.os,
			kernel = excluded.kernel,
			uptime_seconds = excluded.uptime_seconds,
			last_seen = excluded.last_seen
		RETURNING id
	`, hostname, os, kernel, uptimeSeconds, now).Scan(&hostID)
	if err != nil {
		return fmt.Errorf("upsert host: %w", err)
	}

	// Replace containers: delete old, insert new
	if _, err := tx.Exec(`DELETE FROM containers WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("delete containers: %w", err)
	}
	for _, c := range containers {
		if _, err := tx.Exec(`INSERT INTO containers (host_id, name, image, state, last_seen) VALUES (?, ?, ?, ?, ?)`,
			hostID, c.Name, c.Image, c.State, now); err != nil {
			return fmt.Errorf("insert container: %w", err)
		}
	}

	// Replace agents: delete old, insert new
	if _, err := tx.Exec(`DELETE FROM agents WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("delete agents: %w", err)
	}
	for _, a := range agents {
		if _, err := tx.Exec(`INSERT INTO agents (host_id, name, agent_type, status, last_seen) VALUES (?, ?, ?, ?, ?)`,
			hostID, a.Name, a.AgentType, a.Status, now); err != nil {
			return fmt.Errorf("insert agent: %w", err)
		}
	}

	if err := recordSamples(tx, now, hostname, containers, agents); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) AllHosts() ([]Host, error) {
	rows, err := s.DB.Query(`SELECT id, hostname, os, kernel, uptime_seconds, last_seen FROM hosts ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.Hostname, &h.OS, &h.Kernel, &h.UptimeSeconds, &h.LastSeen); err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
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
	rows, err := s.DB.Query(`SELECT id, host_id, name, image, state, last_seen FROM containers WHERE host_id = ? ORDER BY name`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cs []Container
	for rows.Next() {
		var c Container
		if err := rows.Scan(&c.ID, &c.HostID, &c.Name, &c.Image, &c.State, &c.LastSeen); err != nil {
			return nil, err
		}
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
