package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

func broadcastHostConfigs(store *db.Store, hub *sse.Hub) {
	cfgs, err := store.AllHostConfigs()
	if err != nil {
		return
	}
	data, _ := json.Marshal(cfgs)
	hub.Broadcast("host-configs", data)
}

// GetHostConfigs returns all host configs.
// GET /api/host-configs
func GetHostConfigs(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfgs, err := store.AllHostConfigs()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Filter by user access
		hosts, _ := hostsForRequest(store, r)
		if hosts != nil {
			cfgs = filterHostConfigs(cfgs, hosts)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfgs)
	}
}

// UpdateHostConfig upserts config for a single host.
// PUT /api/host-config
func UpdateHostConfig(store *db.Store, hub *sse.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Hostname      string `json:"hostname"`
			ImagePresetID *int64 `json:"image_preset_id"`
			Comment       string `json:"comment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Hostname == "" {
			http.Error(w, "hostname required", http.StatusBadRequest)
			return
		}
		if err := store.UpsertHostConfig(body.Hostname, body.ImagePresetID, body.Comment); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		broadcastHostConfigs(store, hub)
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListImagePresets returns all presets (no image data).
// GET /api/image-presets
func ListImagePresets(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presets, err := store.ListImagePresets()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(presets)
	}
}

// GetImagePresetImage serves the raw image data.
// GET /api/image-presets/{id}/image
func GetImagePresetImage(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		data, mime, err := store.GetImagePresetData(id)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if data == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	}
}

// UploadImagePreset handles multipart upload of a preset image.
// POST /api/image-presets  (multipart: name + file)
func UploadImagePreset(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 512KB max
		if err := r.ParseMultipartForm(512 << 10); err != nil {
			http.Error(w, "file too large or bad form", http.StatusBadRequest)
			return
		}
		name := r.FormValue("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, 512<<10))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		mime := header.Header.Get("Content-Type")
		if mime == "" {
			mime = "image/png"
		}

		id, err := store.CreateImagePreset(name, mime, data)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{"id": id})
	}
}

// DeleteImagePreset removes a preset by ID.
// DELETE /api/image-presets/{id}
func DeleteImagePreset(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if err := store.DeleteImagePreset(id); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
