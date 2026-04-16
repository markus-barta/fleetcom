package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/markus-barta/fleetcom/internal/db"
)

type manifestEntry struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Filename string `json:"filename"`
}

// ExportImagePresets creates a ZIP bundle of all image presets and downloads it.
// GET /api/image-presets/export
func ExportImagePresets(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presets, err := store.AllImagePresetsWithData()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)

		var manifest []manifestEntry
		for _, p := range presets {
			ext := extFromMime(p.MimeType)
			filename := sanitizeFilename(p.Name) + ext
			manifest = append(manifest, manifestEntry{
				Name:     p.Name,
				MimeType: p.MimeType,
				Filename: filename,
			})

			fw, err := zw.Create(filename)
			if err != nil {
				http.Error(w, "zip error", http.StatusInternalServerError)
				return
			}
			if _, err := fw.Write(p.Data); err != nil {
				http.Error(w, "zip error", http.StatusInternalServerError)
				return
			}
		}

		// Write manifest
		mw, err := zw.Create("manifest.json")
		if err != nil {
			http.Error(w, "zip error", http.StatusInternalServerError)
			return
		}
		enc := json.NewEncoder(mw)
		enc.SetIndent("", "  ")
		enc.Encode(manifest)

		if err := zw.Close(); err != nil {
			http.Error(w, "zip error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="fleetcom-icons.zip"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
		w.Write(buf.Bytes())
	}
}

// ImportImagePresets accepts a ZIP bundle and adds/overwrites icon presets.
// POST /api/image-presets/import?mode=append|overwrite (default: append)
func ImportImagePresets(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "append"
		}

		// 10MB max
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "file too large", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, 10<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			http.Error(w, "invalid zip file", http.StatusBadRequest)
			return
		}

		// Read manifest
		var manifest []manifestEntry
		for _, f := range zr.File {
			if f.Name == "manifest.json" {
				rc, err := f.Open()
				if err != nil {
					http.Error(w, "cannot read manifest", http.StatusBadRequest)
					return
				}
				json.NewDecoder(rc).Decode(&manifest)
				rc.Close()
				break
			}
		}

		if len(manifest) == 0 {
			http.Error(w, "no manifest.json in zip", http.StatusBadRequest)
			return
		}

		// Build filename→data map
		fileData := make(map[string][]byte)
		for _, f := range zr.File {
			if f.Name == "manifest.json" {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			d, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			fileData[f.Name] = d
		}

		imported := 0
		for _, entry := range manifest {
			imgData, ok := fileData[entry.Filename]
			if !ok {
				continue
			}

			existingID, _ := store.GetImagePresetByName(entry.Name)
			if existingID > 0 && mode == "overwrite" {
				store.UpdateImagePreset(existingID, entry.MimeType, imgData)
				imported++
			} else if existingID == 0 {
				store.CreateImagePreset(entry.Name, entry.MimeType, imgData)
				imported++
			}
			// append mode + exists → skip (already exists)
		}

		writeJSON(w, map[string]int{"imported": imported, "total": len(manifest)})
	}
}

func extFromMime(mime string) string {
	switch {
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "jpeg"), strings.Contains(mime, "jpg"):
		return ".jpg"
	case strings.Contains(mime, "gif"):
		return ".gif"
	case strings.Contains(mime, "svg"):
		return ".svg"
	case strings.Contains(mime, "webp"):
		return ".webp"
	default:
		return ".bin"
	}
}

func sanitizeFilename(name string) string {
	// Remove path separators and non-safe chars
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		name = "unnamed"
	}
	// Strip any existing extension (we add our own from mime)
	if idx := strings.LastIndex(name, "."); idx > 0 {
		name = name[:idx]
	}
	return name
}
