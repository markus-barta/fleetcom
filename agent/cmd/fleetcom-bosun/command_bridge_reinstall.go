package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// handleBridgeReinstall is bridge.uninstall + bridge.install in one
// bosun action (FLEET-131). Atomic from the operator's perspective:
// one click in the dashboard, one row in the command-result history,
// no 60s heartbeat-cycle gap between two queued commands.
//
// FLEET-149: when params don't specify agent_names, harvest them from
// the existing container's BRIDGE_AGENT_NAMES env. Otherwise reinstall
// would hit bridge.install's "agent_names required" guard or worse
// (pre-149 behaviour) silently default to merlin,nimue and pollute
// FleetCom with agent identities that don't belong on this host.
func handleBridgeReinstall(id int64, params json.RawMessage) (json.RawMessage, error) {
	// Decode params first so we can fill in agent_names from the running
	// container if the caller didn't provide them.
	var p bridgeInstallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if p.ContainerName == "" {
		p.ContainerName = "fleetcom-agent-bridge"
	}

	if strings.TrimSpace(p.AgentNames) == "" {
		harvested := harvestBridgeAgentNames(p.ContainerName)
		if harvested != "" {
			p.AgentNames = harvested
		}
	}

	// Re-marshal so both downstream handlers see the (possibly enriched)
	// params. install will still error loudly if agent_names is empty
	// and there was no existing container to harvest from.
	enriched, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal enriched params: %w", err)
	}

	uninstallRes, err := handleBridgeUninstall(id, enriched)
	if err != nil {
		return nil, fmt.Errorf("uninstall step: %w", err)
	}
	installRes, err := handleBridgeInstall(id, enriched)
	if err != nil {
		return nil, fmt.Errorf("install step: %w", err)
	}

	var u, i map[string]any
	_ = json.Unmarshal(uninstallRes, &u)
	_ = json.Unmarshal(installRes, &i)
	return json.Marshal(map[string]any{
		"uninstall": u,
		"install":   i,
		"message":   "bridge reinstalled",
	})
}

// harvestBridgeAgentNames returns the value of BRIDGE_AGENT_NAMES from
// the named container's existing env, or "" if the container is absent
// or has no such env var. Used by handleBridgeReinstall (FLEET-149) to
// preserve operator-asserted agent identity across reinstalls without
// requiring the dashboard / command issuer to repeat them.
func harvestBridgeAgentNames(containerName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		`{{range .Config.Env}}{{println .}}{{end}}`,
		containerName,
	).CombinedOutput()
	if err != nil {
		return ""
	}
	const prefix = "BRIDGE_AGENT_NAMES="
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}
