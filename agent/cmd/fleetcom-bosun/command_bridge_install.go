package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// bridgeInstallParams is the shape FleetCom sends for bridge.install.
// The FleetCom-side per-host token is NOT a param — bosun reuses its own
// FLEETCOM_TOKEN env var (same per-host bearer the bridge needs to POST
// /api/bridges/register).
//
// The gateway's auth.token shared-secret is also NOT a param (after
// FLEET-134). Bosun discovers it locally by inspecting the gateway
// container for a bind-mount at /run/secrets/gateway-token, and mirrors
// the same bind-mount into the bridge — secret never leaves the host.
// GatewayOperatorToken below remains as a dev/test override only.
type bridgeInstallParams struct {
	AgentNames           string `json:"agent_names"`            // comma-separated, e.g. "merlin,nimue"
	AgentType            string `json:"agent_type"`             // default "openclaw"
	GatewayURL           string `json:"gateway_url"`            // default "wss://localhost:18789"
	Image                string `json:"image"`                  // default "ghcr.io/markus-barta/fleetcom-agent-bridge:latest"
	ContainerName        string `json:"container_name"`         // default "fleetcom-agent-bridge"
	VolumeName           string `json:"volume_name"`            // default "fleetcom-agent-bridge-keys"
	GatewayContainer     string `json:"gateway_container"`      // default "openclaw-gateway" — also used by FLEET-134 to locate the gateway-token bind-mount
	GatewayOperatorToken string `json:"gateway_operator_token"` // dev/test override; production hosts use the FLEET-134 bind-mount path
}

// handleBridgeInstall stands up the agent-bridge container on this host.
// Idempotent: if the container already exists, returns success without
// creating a duplicate. Uses docker run with --network host so the
// bridge can reach the sibling gateway on localhost:18789 without
// depending on a specific compose network.
func handleBridgeInstall(_ int64, params json.RawMessage) (json.RawMessage, error) {
	var p bridgeInstallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	// Defaults.
	if p.AgentNames == "" {
		p.AgentNames = "merlin,nimue"
	}
	if p.AgentType == "" {
		p.AgentType = "openclaw"
	}
	if p.GatewayURL == "" {
		p.GatewayURL = "wss://localhost:18789"
	}
	if p.Image == "" {
		p.Image = "ghcr.io/markus-barta/fleetcom-agent-bridge:latest"
	}
	if p.ContainerName == "" {
		p.ContainerName = "fleetcom-agent-bridge"
	}
	if p.VolumeName == "" {
		p.VolumeName = "fleetcom-agent-bridge-keys"
	}
	if p.GatewayContainer == "" {
		p.GatewayContainer = "openclaw-gateway"
	}

	// Pull secrets from bosun's own env — never trust the command queue
	// to carry a token. The bridge uses the host's bosun token since
	// it's a per-host bearer (bridge POSTs /api/bridges/register with it).
	token := os.Getenv("FLEETCOM_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("FLEETCOM_TOKEN not set in bosun env — can't deploy a bridge without it")
	}
	fleetcomURL := os.Getenv("FLEETCOM_URL")
	if fleetcomURL == "" {
		fleetcomURL = "https://fleet.barta.cm"
	}
	hostname := os.Getenv("FLEETCOM_HOSTNAME")
	if hostname == "" {
		h, _ := os.Hostname()
		hostname = h
	}

	// Readiness check: no point deploying a bridge where there's no
	// gateway to talk to. Fails cleanly rather than leaving a crash-
	// looping container.
	if err := ensureContainerRunning(p.GatewayContainer); err != nil {
		return nil, fmt.Errorf("gateway not ready: %w", err)
	}

	// Idempotency: if the bridge container already exists, report that
	// instead of creating a duplicate.
	if exists, running := inspectContainerState(p.ContainerName); exists {
		msg := "already installed"
		if !running {
			// Dead or stopped — start it.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "docker", "start", p.ContainerName).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("restart existing bridge: %v · %s", err, strings.TrimSpace(string(out)))
			}
			msg = "restarted existing container"
		}
		return json.Marshal(map[string]any{"container": p.ContainerName, "message": msg})
	}

	// Pull the image up-front so any registry issue surfaces before we
	// try to create the container.
	pullCtx, pcancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer pcancel()
	if out, err := exec.CommandContext(pullCtx, "docker", "pull", p.Image).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pull %s: %v · %s", p.Image, err, strings.TrimSpace(string(out)))
	}

	// FLEET-134: discover the gateway container's bind-mount source for
	// /run/secrets/gateway-token (the openclaw shared-secret loaded from
	// /run/agenix/<host>-ocean-gateway-token at provisioning time). When
	// found, mirror the same bind-mount into the bridge container. The
	// bridge then sends the value as auth.token in its connect frame and
	// gets in via openclaw's role=operator + sharedAuthOk short-circuit
	// (no device-pairing dance). The secret never leaves the host
	// filesystem; bosun never reads its contents.
	gatewayTokenSrc := findGatewayTokenSource(p.GatewayContainer)

	// docker run with host network so the bridge can reach the sibling
	// openclaw-gateway on localhost without depending on a specific
	// compose project.
	runCtx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rcancel()
	args := []string{"run", "-d",
		"--name", p.ContainerName,
		"--restart", "unless-stopped",
		"--network", "host",
		"-v", p.VolumeName + ":/var/lib/fleetcom-agent-bridge",
		"-e", "FLEETCOM_URL=" + fleetcomURL,
		"-e", "FLEETCOM_TOKEN=" + token,
		"-e", "FLEETCOM_HOSTNAME=" + hostname,
		"-e", "OPENCLAW_GATEWAY_URL=" + p.GatewayURL,
		"-e", "BRIDGE_AGENT_NAMES=" + p.AgentNames,
		"-e", "BRIDGE_AGENT_TYPE=" + p.AgentType,
		"-l", "com.centurylinklabs.watchtower.enable=true",
	}
	if gatewayTokenSrc != "" {
		args = append(args, "-v", gatewayTokenSrc+":/run/secrets/gateway-token:ro")
	}
	if p.GatewayOperatorToken != "" {
		// Dev/test fallback (FLEET-129). Path 5 (the bind-mount above)
		// is the canonical source on production hosts.
		args = append(args, "-e", "BRIDGE_OPERATOR_TOKEN="+p.GatewayOperatorToken)
	}
	args = append(args, p.Image)
	out, err := exec.CommandContext(runCtx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %v · %s", err, strings.TrimSpace(string(out)))
	}
	return json.Marshal(map[string]any{
		"container":    p.ContainerName,
		"container_id": strings.TrimSpace(string(out)),
		"message":      "bridge deployed",
	})
}

// inspectContainerState returns (exists, running) — used by the idempotency
// check so re-issuing bridge.install is safe.
func inspectContainerState(name string) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name).CombinedOutput()
	if err != nil {
		return false, false
	}
	return true, strings.TrimSpace(string(out)) == "true"
}

// findGatewayTokenSource returns the host-side path of the openclaw
// gateway container's bind-mount for /run/secrets/gateway-token, or ""
// if the gateway container has no such mount (e.g. token-mode auth not
// configured). Used by bridge.install (FLEET-134) to mirror the same
// bind-mount into the bridge container so the bridge can authenticate
// against the gateway via the shared-token short-circuit.
//
// Empty result is a normal outcome — the bridge just falls back to its
// other token sources. We never read the file contents from this code
// path; only the host-side path string crosses bosun's process memory.
func findGatewayTokenSource(gatewayContainer string) string {
	if gatewayContainer == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		`{{range .Mounts}}{{if eq .Destination "/run/secrets/gateway-token"}}{{.Source}}{{end}}{{end}}`,
		gatewayContainer,
	).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
