package db

import (
	"database/sql"
	"fmt"
)

// AlertState is one dedupe/recovery row for a rule and entity.
// Status is "ok", "pending", or "firing". Pending means the condition
// was true but the notification failed, so the evaluator should retry.
type AlertState struct {
	ID          int64
	RuleKey     string
	EntityType  string
	EntityKey   string
	Status      string
	Title       string
	Summary     string
	ActiveSince string
	ResolvedAt  string
	LastSentAt  string
	LastError   string
	UpdatedAt   string
}

// AlertStateUpdate carries the full replacement for an alert state row.
type AlertStateUpdate struct {
	RuleKey     string
	EntityType  string
	EntityKey   string
	Status      string
	Title       string
	Summary     string
	ActiveSince string
	ResolvedAt  string
	LastSentAt  string
	LastError   string
	UpdatedAt   string
}

func (s *Store) AlertState(ruleKey, entityKey string) (*AlertState, error) {
	var st AlertState
	err := s.DB.QueryRow(`
		SELECT id, rule_key, entity_type, entity_key, status, title, summary,
		       active_since, resolved_at, last_sent_at, last_error, updated_at
		FROM alert_states
		WHERE rule_key = ? AND entity_key = ?
	`, ruleKey, entityKey).Scan(
		&st.ID, &st.RuleKey, &st.EntityType, &st.EntityKey, &st.Status,
		&st.Title, &st.Summary, &st.ActiveSince, &st.ResolvedAt,
		&st.LastSentAt, &st.LastError, &st.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("alert state: %w", err)
	}
	return &st, nil
}

func (s *Store) UpsertAlertState(in AlertStateUpdate) error {
	_, err := s.DB.Exec(`
		INSERT INTO alert_states (
			rule_key, entity_type, entity_key, status, title, summary,
			active_since, resolved_at, last_sent_at, last_error, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_key, entity_key) DO UPDATE SET
			entity_type = excluded.entity_type,
			status = excluded.status,
			title = excluded.title,
			summary = excluded.summary,
			active_since = excluded.active_since,
			resolved_at = excluded.resolved_at,
			last_sent_at = excluded.last_sent_at,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at
	`, in.RuleKey, in.EntityType, in.EntityKey, in.Status, in.Title, in.Summary,
		in.ActiveSince, in.ResolvedAt, in.LastSentAt, in.LastError, in.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert alert state: %w", err)
	}
	return nil
}
