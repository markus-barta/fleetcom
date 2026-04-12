package db

import (
	"database/sql"
	"fmt"
)

type HostConfig struct {
	Hostname      string `json:"hostname"`
	ImagePresetID *int64 `json:"image_preset_id"`
	Comment       string `json:"comment"`
}

type ImagePreset struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	MimeType  string `json:"mime_type"`
	CreatedAt string `json:"created_at"`
}

// AllHostConfigs returns config for every host that has one.
func (s *Store) AllHostConfigs() (map[string]HostConfig, error) {
	rows, err := s.DB.Query(`SELECT hostname, image_preset_id, comment FROM host_configs`)
	if err != nil {
		return nil, fmt.Errorf("query host configs: %w", err)
	}
	defer rows.Close()

	out := make(map[string]HostConfig)
	for rows.Next() {
		var hc HostConfig
		if err := rows.Scan(&hc.Hostname, &hc.ImagePresetID, &hc.Comment); err != nil {
			return nil, fmt.Errorf("scan host config: %w", err)
		}
		out[hc.Hostname] = hc
	}
	return out, rows.Err()
}

// UpsertHostConfig creates or updates config for a host.
func (s *Store) UpsertHostConfig(hostname string, imagePresetID *int64, comment string) error {
	_, err := s.DB.Exec(`
		INSERT INTO host_configs (hostname, image_preset_id, comment) VALUES (?, ?, ?)
		ON CONFLICT(hostname) DO UPDATE SET
			image_preset_id = excluded.image_preset_id,
			comment = excluded.comment
	`, hostname, imagePresetID, comment)
	if err != nil {
		return fmt.Errorf("upsert host config: %w", err)
	}
	return nil
}

// ListImagePresets returns all presets (without blob data).
func (s *Store) ListImagePresets() ([]ImagePreset, error) {
	rows, err := s.DB.Query(`SELECT id, name, mime_type, created_at FROM image_presets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list presets: %w", err)
	}
	defer rows.Close()

	out := []ImagePreset{}
	for rows.Next() {
		var p ImagePreset
		if err := rows.Scan(&p.ID, &p.Name, &p.MimeType, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan preset: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetImagePresetData returns the raw image bytes and mime type.
func (s *Store) GetImagePresetData(id int64) ([]byte, string, error) {
	var data []byte
	var mime string
	err := s.DB.QueryRow(`SELECT data, mime_type FROM image_presets WHERE id = ?`, id).Scan(&data, &mime)
	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("get preset data: %w", err)
	}
	return data, mime, nil
}

// CreateImagePreset stores a new preset image.
func (s *Store) CreateImagePreset(name, mimeType string, data []byte) (int64, error) {
	res, err := s.DB.Exec(
		`INSERT INTO image_presets (name, mime_type, data) VALUES (?, ?, ?)`,
		name, mimeType, data,
	)
	if err != nil {
		return 0, fmt.Errorf("create preset: %w", err)
	}
	return res.LastInsertId()
}

// DeleteImagePreset removes a preset by ID.
func (s *Store) DeleteImagePreset(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM image_presets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete preset: %w", err)
	}
	return nil
}
