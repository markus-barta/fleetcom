package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// agentUpdateParams is what the server sends in the agent.update command.
// All three are filled in by the server's pre-flight (FLEET-85): admin
// only sets `target` (default "latest") + optional `source`; the server
// captures `pre_update_version` for post-restart reconciliation.
type agentUpdateParams struct {
	Target           string `json:"target"`
	Source           string `json:"source"`
	PreUpdateVersion string `json:"pre_update_version"`
}

// dockerInspectSpec is what we extract from `docker inspect` on the
// running bosun container — enough to pass into the helper so it can
// reproduce the same `docker run` (for non-compose hosts) or detect a
// compose project (for compose-managed hosts).
type dockerInspectSpec struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Cmd    []string          `json:"Cmd"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Binds         []string                       `json:"Binds"`
		RestartPolicy struct{ Name string }          `json:"RestartPolicy"`
		NetworkMode   string                         `json:"NetworkMode"`
		PortBindings  map[string][]map[string]string `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct{} `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// recreatePayload is what the parent passes to the helper (--recreate-mode)
// via env var. Keeps the helper code tiny and explicit; no shared state
// between processes beyond this JSON.
type recreatePayload struct {
	Target            string            `json:"target"`               // container to stop+recreate
	Image             string            `json:"image"`                // image:tag to run with
	ComposeProject    string            `json:"compose_project"`      // empty if not compose-managed
	ComposeWorkingDir string            `json:"compose_working_dir"`  // host path containing docker-compose.yml
	ComposeService    string            `json:"compose_service"`      // service name in the compose file
	BareEnv           []string          `json:"bare_env,omitempty"`   // for non-compose recreate only
	BareBinds         []string          `json:"bare_binds,omitempty"` // for non-compose recreate only
	BareLabels        map[string]string `json:"bare_labels,omitempty"`
	BareRestartPolicy string            `json:"bare_restart_policy,omitempty"`
	BareNetworkMode   string            `json:"bare_network_mode,omitempty"`
}

// handleAgentUpdate dispatches the universal agent.update command for
// docker-bare hosts (FLEET-86). docker+watchtower hosts use the legacy
// watchtower path; systemd-native hosts use FLEET-87's binary-swap.
//
// Flow:
//
//  1. Detect this host's deployment shape so we can refuse early on
//     watchtower / systemd-native hosts (those are handled separately).
//  2. Inspect ourselves to capture the running container's spec.
//  3. docker pull the new image.
//  4. Spawn a helper container (same image, --recreate-mode flag) with
//     the captured spec passed in via env var. Helper waits a few
//     seconds then docker-stops+recreates this container — that kills
//     us. Done.
//
// Reports status="restarting" via the existing command-result endpoint
// before returning errHandlerAlreadyReported so runAndReport doesn't
// overwrite that with a "done".
func handleAgentUpdate(id int64, params json.RawMessage) (json.RawMessage, error) {
	var ps agentUpdateParams
	if err := json.Unmarshal(params, &ps); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	if ps.Target == "" {
		ps.Target = "latest"
	}

	socketPath := dockerSocketPath()
	containers := listContainers(socketPath)
	shape := detectDeploymentShape(socketPath, containers)
	if shape != "docker-bare" {
		return nil, fmt.Errorf("agent.update: this host's shape is %q; FLEET-86 only handles docker-bare (watchtower hosts: legacy path; systemd-native: FLEET-87)", shape)
	}

	selfName := os.Getenv("HOSTNAME") // Docker sets HOSTNAME to the container ID's short form by default
	if v := os.Getenv("FLEETCOM_CONTAINER_NAME"); v != "" {
		selfName = v
	}
	spec, err := inspectSelf(selfName)
	if err != nil {
		return nil, fmt.Errorf("inspect self (%s): %w", selfName, err)
	}

	// Resolve the friendly container name from inspect (Docker prepends
	// a slash). The helper needs this to docker-stop/rm the right thing.
	containerName := strings.TrimPrefix(spec.Name, "/")

	// Determine target image. If the admin asked for a pinned tag/digest,
	// use it; otherwise stay on the same repo with :latest (the common
	// case for staged rollouts).
	targetImage := spec.Config.Image
	if !strings.Contains(targetImage, ":") && !strings.Contains(targetImage, "@") {
		targetImage += ":latest"
	}
	if ps.Target != "" && ps.Target != "latest" {
		// Admin specified a tag or digest. Replace whatever was in the
		// inspect with the requested one but keep the same repository.
		repo := stripTag(targetImage)
		if strings.HasPrefix(ps.Target, "sha256:") {
			targetImage = repo + "@" + ps.Target
		} else {
			targetImage = repo + ":" + ps.Target
		}
	}

	log.Printf("agent.update: pulling %s", targetImage)
	if out, err := exec.Command("docker", "pull", targetImage).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker pull %s: %v: %s", targetImage, err, strings.TrimSpace(string(out)))
	}

	payload := buildRecreatePayload(spec, containerName, targetImage)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal recreate payload: %w", err)
	}

	helperArgs := buildHelperArgs(spec, payload, string(payloadJSON))
	log.Printf("agent.update: spawning recreate helper")
	if out, err := exec.Command("docker", helperArgs...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("spawn helper: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Tell the server we've kicked off the restart. The new bosun's
	// first heartbeat will reconcile this command to "done".
	serverURL := strings.TrimRight(os.Getenv("FLEETCOM_URL"), "/")
	token := os.Getenv("FLEETCOM_TOKEN")
	result := json.RawMessage(fmt.Sprintf(`{"phase":"restarting","target_image":%q,"helper_spawned":true}`, targetImage))
	reportResult(serverURL, token, commandResult{
		ID:     id,
		Status: "restarting",
		Result: result,
	})
	// Sentinel — see runAndReport.
	return nil, errHandlerAlreadyReported
}

// stripTag returns the repository portion of an image reference,
// dropping the ":tag" or "@digest" suffix. Preserves registry-style
// host:port prefixes (split on the last colon only when followed by
// no slash).
func stripTag(ref string) string {
	if i := strings.Index(ref, "@"); i != -1 {
		return ref[:i]
	}
	// Rightmost colon, only if everything after it has no '/'.
	if i := strings.LastIndex(ref, ":"); i != -1 && !strings.Contains(ref[i:], "/") {
		return ref[:i]
	}
	return ref
}

// inspectSelf shells out to docker-cli to fetch the running container's
// spec. Uses the CLI rather than raw socket calls because the response
// is a single object (not a stream) and the schema is stable.
func inspectSelf(name string) (*dockerInspectSpec, error) {
	if name == "" {
		return nil, fmt.Errorf("self container name empty (set FLEETCOM_CONTAINER_NAME)")
	}
	out, err := exec.Command("docker", "inspect", name).Output()
	if err != nil {
		return nil, err
	}
	var arr []dockerInspectSpec
	if err := json.Unmarshal(out, &arr); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("inspect returned empty array for %s", name)
	}
	return &arr[0], nil
}

// buildRecreatePayload condenses the inspect output into the minimal
// shape the helper actually needs.
func buildRecreatePayload(spec *dockerInspectSpec, containerName, targetImage string) recreatePayload {
	out := recreatePayload{
		Target: containerName,
		Image:  targetImage,
	}
	labels := spec.Config.Labels
	if proj := labels["com.docker.compose.project"]; proj != "" {
		out.ComposeProject = proj
		out.ComposeWorkingDir = labels["com.docker.compose.project.working_dir"]
		out.ComposeService = labels["com.docker.compose.service"]
		// For compose-managed: the docker-compose file on the host is
		// the source of truth. We don't need env / mounts / networks
		// in the payload — `docker compose up -d <service>` will read
		// them from the file, picking up the new image as a side effect
		// of the pull we already did.
		return out
	}
	// Bare docker run — capture everything.
	out.BareEnv = append([]string{}, spec.Config.Env...)
	out.BareBinds = append([]string{}, spec.HostConfig.Binds...)
	out.BareLabels = make(map[string]string, len(labels))
	for k, v := range labels {
		out.BareLabels[k] = v
	}
	out.BareRestartPolicy = spec.HostConfig.RestartPolicy.Name
	out.BareNetworkMode = spec.HostConfig.NetworkMode
	return out
}

// buildHelperArgs constructs the `docker run` argv that spawns the
// helper container with our captured spec embedded as an env var. The
// helper inherits Docker socket access; for compose-managed hosts we
// bind-mount the compose dir at the SAME absolute path inside the
// helper so `docker compose` resolves relative paths (volumes, .env,
// build contexts) identically to how they'd resolve on the host. Any
// other mount-translation scheme breaks because the daemon — running
// on the host — interprets those paths against the host filesystem.
func buildHelperArgs(spec *dockerInspectSpec, payload recreatePayload, payloadJSON string) []string {
	args := []string{
		"run", "--rm", "-d",
		"--label", "fleetcom.role=recreate-helper",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-e", "FLEETCOM_RECREATE_PAYLOAD=" + payloadJSON,
	}
	if payload.ComposeWorkingDir != "" {
		args = append(args, "-v", payload.ComposeWorkingDir+":"+payload.ComposeWorkingDir+":ro")
	}
	args = append(args, payload.Image, "--recreate-mode")
	return args
}

// runRecreateMode is the entrypoint when bosun is invoked with
// --recreate-mode (FLEET-86). Reads the payload from env, waits a few
// seconds for the parent to flush its "restarting" report, then docker-
// stops + recreates the target container.
func runRecreateMode() {
	raw := os.Getenv("FLEETCOM_RECREATE_PAYLOAD")
	if raw == "" {
		log.Fatal("recreate-mode: FLEETCOM_RECREATE_PAYLOAD is empty")
	}
	var p recreatePayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		log.Fatalf("recreate-mode: bad payload: %v", err)
	}
	if p.Target == "" {
		log.Fatal("recreate-mode: target empty")
	}

	// Give the outgoing bosun a few seconds to flush its restarting
	// report and settle. 5s is well above typical RTT to FleetCom.
	time.Sleep(5 * time.Second)

	log.Printf("recreate-mode: stopping %s", p.Target)
	if out, err := exec.Command("docker", "stop", "-t", "30", p.Target).CombinedOutput(); err != nil {
		log.Fatalf("docker stop %s: %v: %s", p.Target, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("docker", "rm", p.Target).CombinedOutput(); err != nil {
		// Not fatal — `docker compose up -d` will reuse-or-recreate
		// regardless. Log and continue.
		log.Printf("recreate-mode: docker rm %s: %v: %s (continuing)", p.Target, err, strings.TrimSpace(string(out)))
	}

	if p.ComposeProject != "" {
		// Compose-managed: let compose recreate from its file. The
		// pull was already done by the parent; up -d will pick up the
		// new image as the desired state for the service. We chdir
		// into the host compose path (same path inside helper, by
		// design — see buildHelperArgs) so .env auto-loading works.
		if err := os.Chdir(p.ComposeWorkingDir); err != nil {
			log.Fatalf("chdir %s: %v", p.ComposeWorkingDir, err)
		}
		args := []string{
			"compose",
			"-p", p.ComposeProject,
			"up", "-d", p.ComposeService,
		}
		log.Printf("recreate-mode: docker %s (cwd=%s)", strings.Join(args, " "), p.ComposeWorkingDir)
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			log.Fatalf("docker compose up -d %s: %v: %s", p.ComposeService, err, strings.TrimSpace(string(out)))
		}
		log.Printf("recreate-mode: %s recreated via compose", p.Target)
		return
	}

	// Bare docker run — rebuild the same container with the new image.
	args := []string{"run", "-d", "--name", p.Target}
	if p.BareNetworkMode != "" && p.BareNetworkMode != "default" {
		args = append(args, "--network", p.BareNetworkMode)
	}
	if p.BareRestartPolicy != "" && p.BareRestartPolicy != "no" {
		args = append(args, "--restart", p.BareRestartPolicy)
	}
	for _, e := range p.BareEnv {
		args = append(args, "-e", e)
	}
	for _, b := range p.BareBinds {
		args = append(args, "-v", b)
	}
	for k, v := range p.BareLabels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, p.Image)
	log.Printf("recreate-mode: docker run %s ...", p.Image)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		log.Fatalf("docker run %s: %v: %s", p.Image, err, strings.TrimSpace(string(out)))
	}
	log.Printf("recreate-mode: %s recreated via docker run", p.Target)
}
