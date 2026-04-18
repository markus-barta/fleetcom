package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// runFastfetch executes the bundled fastfetch binary in JSON-output mode
// and returns the raw JSON document. Returns nil on any failure (binary
// missing, non-zero exit, malformed output) — fastfetch is optional.
func runFastfetch(timeout time.Duration) json.RawMessage {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// --pipe: no TTY escapes, stable for capture
	// --format json: machine-readable module output
	cmd := exec.CommandContext(ctx, "fastfetch", "--pipe", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	// Validate — fastfetch prints a JSON array of modules.
	if !json.Valid(out) {
		return nil
	}
	return json.RawMessage(out)
}
