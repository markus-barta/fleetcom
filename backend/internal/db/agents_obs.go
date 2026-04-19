package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ---------- Wire types (mirror docs/AGENT-OBSERVABILITY.md schema v1) ----------

// AgentSnapshot is the per-agent state returned by an exporter's
// /v1/agent-state endpoint and carried inside a heartbeat payload.
type AgentSnapshot struct {
	Host             string               `json:"host"`
	Name             string               `json:"name"`
	AgentType        string               `json:"agent_type,omitempty"`
	Status           string               `json:"status"`
	StatusSince      string               `json:"status_since"`
	CurrentTurnID    string               `json:"current_turn_id,omitempty"`
	Typing           *AgentTyping         `json:"typing,omitempty"`
	LastReplyPerChat map[string]string    `json:"last_reply_per_chat,omitempty"`
	LastError        *AgentErrorSummary   `json:"last_error,omitempty"`
	Rollups24h       *AgentRollups24h     `json:"rollups_24h,omitempty"`
	ConfigDigest     string               `json:"config_digest,omitempty"`
	Config           *AgentSnapshotConfig `json:"config,omitempty"`
}

type AgentTyping struct {
	Active    bool   `json:"active"`
	ChatID    string `json:"chat_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type AgentErrorSummary struct {
	Class       string `json:"class"`
	TS          string `json:"ts"`
	MessageHash string `json:"message_hash,omitempty"`
	Message     string `json:"message,omitempty"`
}

type AgentRollups24h struct {
	Turns             int     `json:"turns"`
	Errors            int     `json:"errors"`
	AvgTurnDurationMs float64 `json:"avg_turn_duration_ms"`
	P95TurnDurationMs float64 `json:"p95_turn_duration_ms"`
}

type AgentSnapshotConfig struct {
	Model             string `json:"model,omitempty"`
	PromptVersion     string `json:"prompt_version,omitempty"`
	EmitExcerpts      bool   `json:"emit_excerpts,omitempty"`
	StuckThresholdSec int    `json:"stuck_threshold_sec,omitempty"`
	StuckSilenceSec   int    `json:"stuck_silence_sec,omitempty"`
}

// AgentEvent is the wire format for POST /api/agent-events.
type AgentEvent struct {
	Agent   AgentRef        `json:"agent"`
	TS      string          `json:"ts"`
	Kind    string          `json:"kind"`
	TurnID  string          `json:"turn_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type AgentRef struct {
	Host string `json:"host"`
	Name string `json:"name"`
}

// Payload shapes (subset — server only strictly needs a few fields per
// event kind; the rest stays in payload_json for future use).

type TurnStartedPayload struct {
	ChatID   string `json:"chat_id"`
	ChatName string `json:"chat_name,omitempty"`
	Model    string `json:"model,omitempty"`
	Excerpt  string `json:"excerpt,omitempty"`
}

type TurnToolInvokedPayload struct {
	ToolID string `json:"tool_id"`
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
}

type TurnToolCompletedPayload struct {
	ToolID     string `json:"tool_id"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

type TurnRepliedPayload struct {
	DurationMs       int64  `json:"duration_ms"`
	TokensPrompt     int    `json:"tokens_prompt,omitempty"`
	TokensCompletion int    `json:"tokens_completion,omitempty"`
	Excerpt          string `json:"excerpt,omitempty"`
}

type TurnErroredPayload struct {
	Class       string `json:"class"`
	MessageHash string `json:"message_hash,omitempty"`
	Message     string `json:"message,omitempty"`
}

// ---------- Read-side types returned by /api/agents ----------

type AgentSummary struct {
	Host       string         `json:"host"`
	Name       string         `json:"name"`
	AgentType  string         `json:"agent_type,omitempty"`
	Snapshot   *AgentSnapshot `json:"snapshot,omitempty"`
	SnapshotAt string         `json:"snapshot_at,omitempty"`
}

type AgentDetail struct {
	AgentSummary
	RecentTurns []AgentTurnRow `json:"recent_turns"`
	RecentTools []AgentToolRow `json:"recent_tools"`
}

type AgentTurnRow struct {
	ID               string  `json:"id"`
	ChatID           string  `json:"chat_id"`
	ChatName         string  `json:"chat_name"`
	StartedAt        string  `json:"started_at"`
	FirstTokenAt     *string `json:"first_token_at,omitempty"`
	RepliedAt        *string `json:"replied_at,omitempty"`
	Status           string  `json:"status"`
	Model            string  `json:"model,omitempty"`
	TokensPrompt     *int    `json:"tokens_prompt,omitempty"`
	TokensCompletion *int    `json:"tokens_completion,omitempty"`
	DurationMs       *int64  `json:"duration_ms,omitempty"`
	ErrorClass       *string `json:"error_class,omitempty"`
	Excerpt          string  `json:"excerpt,omitempty"`
}

type AgentToolRow struct {
	ID          string  `json:"id"`
	TurnID      string  `json:"turn_id"`
	Name        string  `json:"name"`
	Target      string  `json:"target,omitempty"`
	StartedAt   string  `json:"started_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
	ExitCode    *int    `json:"exit_code,omitempty"`
	DurationMs  *int64  `json:"duration_ms,omitempty"`
}

// ---------- Store methods ----------

// UpsertAgentSnapshot stores the latest snapshot for one agent. Returns
// the agent_id and whether the row was freshly inserted.
func (s *Store) UpsertAgentSnapshot(hostname string, snap AgentSnapshot) (int64, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	var hostID int64
	if err := s.DB.QueryRow(`SELECT id FROM hosts WHERE hostname = ?`, hostname).Scan(&hostID); err != nil {
		return 0, false, fmt.Errorf("host not found: %w", err)
	}

	blob, err := json.Marshal(snap)
	if err != nil {
		return 0, false, fmt.Errorf("marshal snapshot: %w", err)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var agentID int64
	err = tx.QueryRow(`
		INSERT INTO agents_obs (host_id, name, agent_type, snapshot_json, snapshot_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(host_id, name) DO UPDATE SET
			agent_type = excluded.agent_type,
			snapshot_json = excluded.snapshot_json,
			snapshot_at = excluded.snapshot_at
		RETURNING id
	`, hostID, snap.Name, snap.AgentType, string(blob), now).Scan(&agentID)
	if err != nil {
		return 0, false, fmt.Errorf("upsert agent: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return agentID, false, nil
}

// InsertAgentEvent writes one event to agent_events and updates the
// agent_turns / agent_tools projections based on the event kind. Runs
// in a single transaction. The agent is resolved by (host, name).
func (s *Store) InsertAgentEvent(ev AgentEvent) error {
	if ev.Agent.Host == "" || ev.Agent.Name == "" || ev.Kind == "" {
		return fmt.Errorf("invalid event: missing agent or kind")
	}
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Resolve agent_id, auto-creating the agent row on first event if
	// no snapshot has arrived yet. Keeps the event stream forward-
	// compatible: exporters can emit events before their first scrape.
	var agentID int64
	row := tx.QueryRow(
		`SELECT a.id FROM agents_obs a JOIN hosts h ON a.host_id = h.id WHERE h.hostname = ? AND a.name = ?`,
		ev.Agent.Host, ev.Agent.Name,
	)
	err = row.Scan(&agentID)
	if err == sql.ErrNoRows {
		var hostID int64
		if err := tx.QueryRow(`SELECT id FROM hosts WHERE hostname = ?`, ev.Agent.Host).Scan(&hostID); err != nil {
			return fmt.Errorf("host %q not registered: %w", ev.Agent.Host, err)
		}
		err = tx.QueryRow(`
			INSERT INTO agents_obs (host_id, name, snapshot_json, snapshot_at)
			VALUES (?, ?, '', '')
			ON CONFLICT(host_id, name) DO UPDATE SET name = excluded.name
			RETURNING id
		`, hostID, ev.Agent.Name).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("create agent row: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("resolve agent: %w", err)
	}

	payloadJSON := string(ev.Payload)
	if payloadJSON == "" {
		payloadJSON = "{}"
	}

	if _, err := tx.Exec(
		`INSERT INTO agent_events (agent_id, ts, kind, turn_id, payload_json) VALUES (?, ?, ?, ?, ?)`,
		agentID, ev.TS, ev.Kind, ev.TurnID, payloadJSON,
	); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	if err := applyEventToProjections(tx, agentID, ev); err != nil {
		return fmt.Errorf("apply event: %w", err)
	}

	return tx.Commit()
}

// applyEventToProjections updates agent_turns / agent_tools based on
// the event kind. Unknown kinds are stored in agent_events but produce
// no projection update — forward compatibility.
func applyEventToProjections(tx *sql.Tx, agentID int64, ev AgentEvent) error {
	switch ev.Kind {
	case "turn.started":
		var p TurnStartedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		_, err := tx.Exec(`
			INSERT INTO agent_turns (id, agent_id, chat_id, chat_name, started_at, status, model, excerpt)
			VALUES (?, ?, ?, ?, ?, 'thinking', ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				chat_id = excluded.chat_id,
				chat_name = excluded.chat_name,
				started_at = excluded.started_at,
				status = 'thinking',
				model = excluded.model,
				excerpt = excluded.excerpt
		`, ev.TurnID, agentID, p.ChatID, p.ChatName, ev.TS, p.Model, p.Excerpt)
		return err

	case "turn.tool-invoked":
		var p TurnToolInvokedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if _, err := tx.Exec(`
			INSERT INTO agent_tools (id, turn_id, name, target, started_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				target = excluded.target,
				started_at = excluded.started_at
		`, p.ToolID, ev.TurnID, p.Name, p.Target, ev.TS); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE agent_turns SET status = 'tool-running' WHERE id = ?`, ev.TurnID)
		return err

	case "turn.tool-completed":
		var p TurnToolCompletedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if _, err := tx.Exec(
			`UPDATE agent_tools SET completed_at = ?, exit_code = ?, duration_ms = ? WHERE id = ?`,
			ev.TS, p.ExitCode, p.DurationMs, p.ToolID,
		); err != nil {
			return err
		}
		// After tool completes the agent goes back to thinking (until
		// turn.replied or turn.tool-invoked moves it again).
		_, err := tx.Exec(`UPDATE agent_turns SET status = 'thinking' WHERE id = ? AND status = 'tool-running'`, ev.TurnID)
		return err

	case "turn.replied":
		var p TurnRepliedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		_, err := tx.Exec(`
			UPDATE agent_turns SET
				replied_at = ?,
				status = 'replied',
				duration_ms = ?,
				tokens_prompt = ?,
				tokens_completion = ?,
				excerpt = COALESCE(NULLIF(?, ''), excerpt)
			WHERE id = ?
		`, ev.TS, p.DurationMs, p.TokensPrompt, p.TokensCompletion, p.Excerpt, ev.TurnID)
		return err

	case "turn.errored":
		var p TurnErroredPayload
		_ = json.Unmarshal(ev.Payload, &p)
		_, err := tx.Exec(
			`UPDATE agent_turns SET status = 'error', error_class = ? WHERE id = ?`,
			p.Class, ev.TurnID,
		)
		return err

	case "turn.abandoned":
		_, err := tx.Exec(
			`UPDATE agent_turns SET status = 'abandoned' WHERE id = ? AND status IN ('thinking','tool-running','replying')`,
			ev.TurnID,
		)
		return err
	}
	return nil
}

// ListAgentsForHosts returns a summary row per agent scoped to the
// given hosts. Admin pass all visible host IDs.
func (s *Store) ListAgentsForHosts(hostIDs []int64) ([]AgentSummary, error) {
	if len(hostIDs) == 0 {
		return []AgentSummary{}, nil
	}
	// Build IN clause. SQLite has no array-bind; easier to query all then filter.
	rows, err := s.DB.Query(`
		SELECT h.hostname, a.name, a.agent_type, a.snapshot_json, a.snapshot_at, a.host_id
		FROM agents_obs a
		JOIN hosts h ON a.host_id = h.id
		ORDER BY h.hostname, a.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowed := make(map[int64]bool, len(hostIDs))
	for _, id := range hostIDs {
		allowed[id] = true
	}
	out := []AgentSummary{}
	for rows.Next() {
		var host, name, agentType, blob, at string
		var hostID int64
		if err := rows.Scan(&host, &name, &agentType, &blob, &at, &hostID); err != nil {
			return nil, err
		}
		if !allowed[hostID] {
			continue
		}
		s := AgentSummary{Host: host, Name: name, AgentType: agentType, SnapshotAt: at}
		if blob != "" {
			var snap AgentSnapshot
			if err := json.Unmarshal([]byte(blob), &snap); err == nil {
				s.Snapshot = &snap
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentDetail returns one agent + recent turns + recent tools for
// dashboard drilldown. Limit defaults to 50 turns / 100 tools.
func (s *Store) AgentDetail(host, name string, turnLimit int) (*AgentDetail, error) {
	if turnLimit <= 0 {
		turnLimit = 50
	}
	var agentID int64
	var agentType, blob, at string
	err := s.DB.QueryRow(`
		SELECT a.id, a.agent_type, a.snapshot_json, a.snapshot_at
		FROM agents_obs a JOIN hosts h ON a.host_id = h.id
		WHERE h.hostname = ? AND a.name = ?
	`, host, name).Scan(&agentID, &agentType, &blob, &at)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	detail := &AgentDetail{
		AgentSummary: AgentSummary{
			Host: host, Name: name, AgentType: agentType, SnapshotAt: at,
		},
		RecentTurns: []AgentTurnRow{},
		RecentTools: []AgentToolRow{},
	}
	if blob != "" {
		var snap AgentSnapshot
		if err := json.Unmarshal([]byte(blob), &snap); err == nil {
			detail.Snapshot = &snap
		}
	}

	trows, err := s.DB.Query(`
		SELECT id, chat_id, chat_name, started_at, first_token_at, replied_at,
		       status, model, tokens_prompt, tokens_completion, duration_ms,
		       error_class, excerpt
		FROM agent_turns WHERE agent_id = ? ORDER BY started_at DESC LIMIT ?
	`, agentID, turnLimit)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var r AgentTurnRow
		var firstTok, replied, errCls sql.NullString
		var tp, tc sql.NullInt64
		var dur sql.NullInt64
		if err := trows.Scan(
			&r.ID, &r.ChatID, &r.ChatName, &r.StartedAt, &firstTok, &replied,
			&r.Status, &r.Model, &tp, &tc, &dur, &errCls, &r.Excerpt,
		); err != nil {
			return nil, err
		}
		if firstTok.Valid {
			v := firstTok.String
			r.FirstTokenAt = &v
		}
		if replied.Valid {
			v := replied.String
			r.RepliedAt = &v
		}
		if errCls.Valid {
			v := errCls.String
			r.ErrorClass = &v
		}
		if tp.Valid {
			v := int(tp.Int64)
			r.TokensPrompt = &v
		}
		if tc.Valid {
			v := int(tc.Int64)
			r.TokensCompletion = &v
		}
		if dur.Valid {
			r.DurationMs = &dur.Int64
		}
		detail.RecentTurns = append(detail.RecentTurns, r)
	}

	// Recent tools across those turns.
	if len(detail.RecentTurns) > 0 {
		ids := make([]any, 0, len(detail.RecentTurns))
		placeholders := ""
		for i, t := range detail.RecentTurns {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			ids = append(ids, t.ID)
		}
		q := `SELECT id, turn_id, name, target, started_at, completed_at, exit_code, duration_ms
		      FROM agent_tools WHERE turn_id IN (` + placeholders + `) ORDER BY started_at DESC LIMIT 200`
		toolRows, err := s.DB.Query(q, ids...)
		if err == nil {
			defer toolRows.Close()
			for toolRows.Next() {
				var r AgentToolRow
				var completed sql.NullString
				var exit sql.NullInt64
				var dur sql.NullInt64
				if err := toolRows.Scan(&r.ID, &r.TurnID, &r.Name, &r.Target, &r.StartedAt, &completed, &exit, &dur); err != nil {
					continue
				}
				if completed.Valid {
					v := completed.String
					r.CompletedAt = &v
				}
				if exit.Valid {
					v := int(exit.Int64)
					r.ExitCode = &v
				}
				if dur.Valid {
					r.DurationMs = &dur.Int64
				}
				detail.RecentTools = append(detail.RecentTools, r)
			}
		}
	}

	return detail, nil
}

// AllHostIDs returns every host id — used for admin SSE re-broadcasts.
func (s *Store) AllHostIDs() ([]int64, error) {
	rows, err := s.DB.Query(`SELECT id FROM hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PruneOldAgentObs deletes events/turns/tools older than retention.
// Snapshot rows in agents_obs are kept (latest-only, no retention).
func (s *Store) PruneOldAgentObs(retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	total := int64(0)
	for _, q := range []string{
		`DELETE FROM agent_events WHERE ts < ?`,
		// turns cascade to tools via FK; prune turns by started_at.
		`DELETE FROM agent_turns WHERE started_at < ?`,
	} {
		res, err := s.DB.Exec(q, cutoff)
		if err != nil {
			return total, fmt.Errorf("prune agent obs: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
