package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
	"github.com/markus-barta/fleetcom/internal/version"
)

// configPayload is the shape broadcast via SSE and returned by GET /api/settings.
type configPayload struct {
	HeartbeatInterval    int    `json:"heartbeat_interval"`
	Commit               string `json:"commit"`
	ExpectedAgentVersion string `json:"expected_agent_version"`
	InstanceLabel        string `json:"instance_label,omitempty"`
	OrgLogoURL           string `json:"org_logo_url,omitempty"`
}

func buildConfigPayload(store *db.Store) configPayload {
	label, _ := store.GetSetting("instance_label", "")
	if label == "" {
		label = os.Getenv("FLEETCOM_INSTANCE_LABEL")
	}

	logoURL := ""
	logo, _ := store.GetSetting("org_logo", "")
	if logo != "" {
		logoURL = "/api/org-logo"
	}

	return configPayload{
		HeartbeatInterval:    store.HeartbeatInterval(),
		Commit:               version.Commit,
		ExpectedAgentVersion: version.AgentVersion,
		InstanceLabel:        label,
		OrgLogoURL:           logoURL,
	}
}

// BroadcastConfig pushes the current config to all SSE clients.
func BroadcastConfig(store *db.Store, hub *sse.Hub) {
	data, _ := json.Marshal(buildConfigPayload(store))
	hub.Broadcast("config", data)
}

// GetSettings returns the current server configuration.
// GET /api/settings (public — agents need the heartbeat interval).
func GetSettings(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildConfigPayload(store))
	}
}

// UpdateSettings accepts a partial config update (admin-only).
// PUT /api/settings
func UpdateSettings(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			HeartbeatInterval *int    `json:"heartbeat_interval"`
			InstanceLabel     *string `json:"instance_label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if body.HeartbeatInterval != nil {
			v := *body.HeartbeatInterval
			if v < 10 || v > 3600 {
				http.Error(w, "heartbeat_interval must be 10–3600", http.StatusBadRequest)
				return
			}
			if err := store.SetSetting("heartbeat_interval", strconv.Itoa(v)); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		if body.InstanceLabel != nil {
			if err := store.SetSetting("instance_label", *body.InstanceLabel); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		// Push new config to all connected browsers
		BroadcastConfig(store, hub)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildConfigPayload(store))
	}
}

// UploadOrgLogo handles POST /api/org-logo (admin-only, multipart).
func UploadOrgLogo(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(256 << 10); err != nil {
			http.Error(w, "file too large", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, 256<<10))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		mime := header.Header.Get("Content-Type")
		if mime == "" {
			mime = "image/png"
		}

		// Store as "mime;base64,data"
		encoded := mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		if err := store.SetSetting("org_logo", encoded); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		BroadcastConfig(store, hub)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// DeleteOrgLogo handles DELETE /api/org-logo (admin-only).
func DeleteOrgLogo(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.SetSetting("org_logo", ""); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		BroadcastConfig(store, hub)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// GetOrgLogo serves the org logo image.
// GET /api/org-logo (public — needed by login page too).
func GetOrgLogo(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		val, _ := store.GetSetting("org_logo", "")
		if val == "" {
			http.Error(w, "no logo", http.StatusNotFound)
			return
		}

		// Parse "mime;base64,data"
		idx := len("image/png;base64,") // minimum
		for i, c := range val {
			if c == ',' {
				idx = i + 1
				break
			}
		}
		mimeEnd := 0
		for i, c := range val {
			if c == ';' {
				mimeEnd = i
				break
			}
		}

		mime := val[:mimeEnd]
		data, err := base64.StdEncoding.DecodeString(val[idx:])
		if err != nil {
			http.Error(w, "corrupt logo data", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(data)
	}
}
