package db

import "fmt"

type IgnoredEntity struct {
	ID         int64  `json:"id"`
	EntityType string `json:"entity_type"`
	EntityKey  string `json:"entity_key"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) AddIgnored(entityType, entityKey string) error {
	_, err := s.DB.Exec(
		`INSERT OR IGNORE INTO ignored_entities (entity_type, entity_key) VALUES (?, ?)`,
		entityType, entityKey,
	)
	if err != nil {
		return fmt.Errorf("add ignored: %w", err)
	}
	return nil
}

func (s *Store) RemoveIgnored(entityType, entityKey string) error {
	_, err := s.DB.Exec(
		`DELETE FROM ignored_entities WHERE entity_type = ? AND entity_key = ?`,
		entityType, entityKey,
	)
	if err != nil {
		return fmt.Errorf("remove ignored: %w", err)
	}
	return nil
}

func (s *Store) ListIgnored() ([]IgnoredEntity, error) {
	rows, err := s.DB.Query(`SELECT id, entity_type, entity_key, created_at FROM ignored_entities ORDER BY entity_type, entity_key`)
	if err != nil {
		return nil, fmt.Errorf("list ignored: %w", err)
	}
	defer rows.Close()

	out := []IgnoredEntity{}
	for rows.Next() {
		var e IgnoredEntity
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityKey, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan ignored: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// IgnoredSet returns all ignored entities as "type:key" strings for fast lookup.
func (s *Store) IgnoredSet() (map[string]bool, error) {
	list, err := s.ListIgnored()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, e := range list {
		out[e.EntityType+":"+e.EntityKey] = true
	}
	return out, nil
}
