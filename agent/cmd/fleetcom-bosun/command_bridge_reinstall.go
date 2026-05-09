package main

import (
	"encoding/json"
	"fmt"
)

// handleBridgeReinstall is bridge.uninstall + bridge.install in one
// bosun action (FLEET-131). Atomic from the operator's perspective:
// one click in the dashboard, one row in the command-result history,
// no 60s heartbeat-cycle gap between two queued commands.
//
// Accepts the union of bridgeInstallParams and bridgeUninstallParams.
// The state volume is preserved by default — reinstalls should not lose
// pairing state. To rotate fingerprints / start fresh, use bridge.install
// after a bridge.uninstall with remove_volume=true.
func handleBridgeReinstall(id int64, params json.RawMessage) (json.RawMessage, error) {
	// Run uninstall first. If the container isn't installed we'll get
	// "not installed" back — that's fine, treat reinstall as install.
	uninstallRes, err := handleBridgeUninstall(id, params)
	if err != nil {
		return nil, fmt.Errorf("uninstall step: %w", err)
	}
	installRes, err := handleBridgeInstall(id, params)
	if err != nil {
		return nil, fmt.Errorf("install step: %w", err)
	}

	// Merge the two results into a single payload so the command-result
	// history captures both phases.
	var u, i map[string]any
	_ = json.Unmarshal(uninstallRes, &u)
	_ = json.Unmarshal(installRes, &i)
	return json.Marshal(map[string]any{
		"uninstall": u,
		"install":   i,
		"message":   "bridge reinstalled",
	})
}
