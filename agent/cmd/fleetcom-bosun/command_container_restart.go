package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// containerRestartParams is the shape FleetCom sends for the
// container.restart command. name is the docker container name
// (not ID). No namespace/host param — bosun only restarts containers
// on its own host, and the server-side routing uses the command's
// `host` field to enforce that.
type containerRestartParams struct {
	Name string `json:"name"`
}

// handleContainerRestart restarts one container by name via the docker
// CLI using the socket bosun already has mounted. Fails fast if the
// container doesn't exist or docker isn't reachable.
func handleContainerRestart(params json.RawMessage) (json.RawMessage, error) {
	var p containerRestartParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return nil, fmt.Errorf("container name required")
	}
	// Guard against shell injection — docker accepts these as a single
	// arg (not via shell) but a stray newline or control char would
	// still be weird to see in logs.
	if strings.ContainsAny(p.Name, " \t\n\r") {
		return nil, fmt.Errorf("invalid container name")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "restart", p.Name).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker restart failed: %v · %s", err, strings.TrimSpace(string(out)))
	}
	// `docker restart` echoes the container name on success.
	result := map[string]any{
		"name":    p.Name,
		"message": strings.TrimSpace(string(out)),
	}
	b, _ := json.Marshal(result)
	return b, nil
}
