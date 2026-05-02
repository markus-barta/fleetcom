package api

import (
	"encoding/json"
	"net/http"

	"github.com/markus-barta/fleetcom/internal/version"
)

type VersionResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	// DestructiveCommandsEnabled (FLEET-369.1) signals whether the server
	// will accept commands like host.reboot. Surfaced here (rather than a
	// new endpoint) so the UI can hide gated buttons without an extra
	// round-trip; tied to FLEETCOM_DESTRUCTIVE_COMMANDS env at request time.
	DestructiveCommandsEnabled bool `json:"destructive_commands_enabled"`
}

func Version(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(VersionResponse{
		Version:                    version.Version,
		Commit:                     version.Commit,
		BuildTime:                  version.BuildTime,
		GoVersion:                  version.GoVersion(),
		DestructiveCommandsEnabled: destructiveCommandsEnabled(),
	})
}
