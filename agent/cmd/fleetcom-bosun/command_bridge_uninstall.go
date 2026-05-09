package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// bridgeUninstallParams configures FLEET-131 bridge.uninstall: stop +
// remove the on-host bridge container. The state volume is preserved
// by default so a subsequent reinstall keeps the persisted operator
// token, identity, etc. Set RemoveVolume=true for a full teardown
// (decommissioning, fingerprint rotation).
type bridgeUninstallParams struct {
	ContainerName string `json:"container_name"` // default "fleetcom-agent-bridge"
	VolumeName    string `json:"volume_name"`    // default "fleetcom-agent-bridge-keys"
	RemoveVolume  bool   `json:"remove_volume"`  // default false — keep state across reinstalls
}

// handleBridgeUninstall is the inverse of bridge.install. Idempotent:
// returns success with a message="not installed" when the container is
// already absent so the dashboard can call it freely without first
// checking state. Volume removal is gated on RemoveVolume so a typical
// reinstall doesn't accidentally drop pairing state.
func handleBridgeUninstall(_ int64, params json.RawMessage) (json.RawMessage, error) {
	var p bridgeUninstallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if p.ContainerName == "" {
		p.ContainerName = "fleetcom-agent-bridge"
	}
	if p.VolumeName == "" {
		p.VolumeName = "fleetcom-agent-bridge-keys"
	}

	exists, running := inspectContainerState(p.ContainerName)
	containerRemoved := false
	if exists {
		// Best-effort stop first so docker rm -f's SIGKILL isn't the
		// shutdown signal a healthy bridge sees on a normal redeploy.
		if running {
			stopCtx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
			if out, err := exec.CommandContext(stopCtx, "docker", "stop", p.ContainerName).CombinedOutput(); err != nil {
				scancel()
				return nil, fmt.Errorf("docker stop failed: %v · %s", err, strings.TrimSpace(string(out)))
			}
			scancel()
		}
		rmCtx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		if out, err := exec.CommandContext(rmCtx, "docker", "rm", p.ContainerName).CombinedOutput(); err != nil {
			rcancel()
			return nil, fmt.Errorf("docker rm failed: %v · %s", err, strings.TrimSpace(string(out)))
		}
		rcancel()
		containerRemoved = true
	}

	volumeRemoved := false
	if p.RemoveVolume {
		// Best-effort: a missing volume is fine, anything else is a
		// real error worth surfacing.
		volCtx, vcancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(volCtx, "docker", "volume", "rm", p.VolumeName).CombinedOutput()
		vcancel()
		if err != nil {
			outStr := strings.TrimSpace(string(out))
			if !strings.Contains(outStr, "no such volume") && !strings.Contains(outStr, "not found") {
				return nil, fmt.Errorf("docker volume rm failed: %v · %s", err, outStr)
			}
		} else {
			volumeRemoved = true
		}
	}

	msg := "not installed"
	switch {
	case containerRemoved && volumeRemoved:
		msg = "container removed, volume deleted"
	case containerRemoved:
		msg = "container removed (volume preserved)"
	case volumeRemoved:
		msg = "container was already absent, volume deleted"
	}

	return json.Marshal(map[string]any{
		"container":         p.ContainerName,
		"container_removed": containerRemoved,
		"volume_removed":    volumeRemoved,
		"message":           msg,
	})
}
