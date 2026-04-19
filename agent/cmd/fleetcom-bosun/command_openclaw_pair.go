package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// openclawPairParams is the shape of the openclaw.pair command's params
// as sent by FleetCom. public_key_pem is FleetCom's operator pubkey
// PEM; operator_token is the shared secret the gateway should accept
// on subsequent operator connects. container_name defaults to
// "openclaw-gateway".
type openclawPairParams struct {
	PublicKeyPEM  string `json:"public_key_pem"`
	OperatorToken string `json:"operator_token"`
	ContainerName string `json:"container_name"`
}

// handleOpenclawPair merges FleetCom's operator entry into OpenClaw's
// /home/node/.openclaw/devices/paired.json inside the gateway
// container, then restarts it so the new entry is picked up on boot.
//
// Implementation uses `docker exec` to run a short Python block inside
// the container because:
//   - the gateway's data dir is a bind-mounted volume; editing from
//     the host would require knowing the exact mount path and whether
//     it's owned by the `node` user (UID 1000 inside the container).
//   - the container already has python3 (used elsewhere in its
//     entrypoint) so the runtime is guaranteed.
//   - it's the same merge shape the FLEET-52 entrypoint writes, kept
//     in sync so pre-seeded + dynamically-paired gateways have
//     identical paired.json entries.
//
// We also preserve any existing paired devices (control UI pairings,
// other FleetCom instances, etc.) — this is a merge, not a replace.
func handleOpenclawPair(params json.RawMessage) (json.RawMessage, error) {
	var p openclawPairParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if p.PublicKeyPEM == "" || p.OperatorToken == "" {
		return nil, fmt.Errorf("public_key_pem and operator_token required")
	}
	container := strings.TrimSpace(p.ContainerName)
	if container == "" {
		container = "openclaw-gateway"
	}

	// Sanity check: does the container exist + is it running?
	if err := ensureContainerRunning(container); err != nil {
		return nil, err
	}

	// Merge paired.json via python3 inside the container. pubkey + token
	// come in as env vars (not command args) so they don't end up in
	// process listings.
	mergeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(mergeCtx,
		"docker", "exec", "-i",
		"-e", "FC_PUBKEY="+p.PublicKeyPEM,
		"-e", "FC_TOKEN="+p.OperatorToken,
		container, "python3", "-c", pairedJSONMergeScript,
	)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("paired.json merge failed: %v · %s", err, strings.TrimSpace(errBuf.String()))
	}
	mergeLine := strings.TrimSpace(out.String())

	// Restart the container so the new paired.json takes effect.
	// OpenClaw reloads devices on boot; hot-reload isn't guaranteed.
	restartCtx, rcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer rcancel()
	if err := exec.CommandContext(restartCtx, "docker", "restart", container).Run(); err != nil {
		return nil, fmt.Errorf("container restart failed: %w", err)
	}

	result := map[string]any{
		"container": container,
		"merge":     mergeLine,
		"restarted": true,
	}
	b, _ := json.Marshal(result)
	return b, nil
}

func ensureContainerRunning(container string) error {
	// First verify the docker CLI itself is available. Without it, we
	// get a misleading exec "not found" that shows up as "container X
	// not found" to the admin. Pre-checking lets us point at the real
	// issue (rebuild bosun image).
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not available in bosun container — rebuild/update the bosun image (needs `docker-cli` package)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", container).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container %q not found: %s", container, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("container %q is not running", container)
	}
	return nil
}

// pairedJSONMergeScript is a small Python heredoc run inside the
// gateway container. Kept in sync with the FLEET-52 nixcfg entrypoint
// merge (hosts/hsb0/docker/openclaw-gateway/entrypoint.sh step 3.5).
// Consumes FC_PUBKEY + FC_TOKEN env vars.
const pairedJSONMergeScript = `
import base64, hashlib, json, os, sys, time
try:
    pem = os.environ["FC_PUBKEY"]
    token = os.environ["FC_TOKEN"].strip()
    # Strip PEM armor, decode DER, slice off SPKI prefix → 32-byte raw Ed25519 pubkey.
    der = base64.b64decode("".join(
        l.strip() for l in pem.splitlines()
        if l.strip() and not l.startswith("-----")
    ))
    raw = der[-32:]
    device_id = hashlib.sha256(raw).hexdigest()
    pub_b64u = base64.urlsafe_b64encode(raw).decode().rstrip("=")

    paired_path = "/home/node/.openclaw/devices/paired.json"
    os.makedirs(os.path.dirname(paired_path), exist_ok=True)
    data = {}
    if os.path.exists(paired_path):
        try:
            data = json.loads(open(paired_path).read() or "{}")
        except Exception:
            data = {}

    now_ms = int(time.time() * 1000)
    entry = data.get(device_id, {})
    existing_token = (entry.get("tokens", {}) or {}).get("operator", {}) or {}
    data[device_id] = {
        "deviceId":       device_id,
        "publicKey":      pub_b64u,
        "platform":       "linux",
        "clientId":       "gateway-client",
        "clientMode":     "backend",
        "role":           "operator",
        "roles":          ["operator"],
        "scopes":         ["operator.read", "operator.pairing"],
        "approvedAt":     entry.get("approvedAt", now_ms),
        "approvedScopes": ["operator.read", "operator.pairing"],
        "tokens": {
            "operator": {
                "token":       token,
                "role":        "operator",
                "scopes":      ["operator.read", "operator.pairing"],
                "createdAtMs": existing_token.get("createdAtMs", now_ms),
            }
        },
    }

    tmp = paired_path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(data, f, indent=2)
    os.replace(tmp, paired_path)
    sys.stdout.write("merged deviceId=" + device_id[:12] + " (" + str(len(data)) + " total entries)")
except Exception as e:
    sys.stderr.write("merge failed: " + str(e))
    sys.exit(1)
`
