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
// running bosun container — enough to reproduce it via plain
// `docker run`. We deliberately do NOT use `docker compose up` for the
// recreate even when the container was originally created by compose:
// compose, running inside the helper, can't resolve env_file paths,
// build contexts, or ${VAR} refs that point outside the project dir
// (FLEET-86 first attempt failed exactly here on hsb0). Reproducing
// from the inspected spec is host-config agnostic. We preserve the
// com.docker.compose.* labels so future `docker compose up` from the
// host still recognises the container as project-managed.
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
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

// recreatePayload is what the parent passes to the helper (--recreate-mode)
// via env var. Single shape for all hosts — no compose/bare branching.
type recreatePayload struct {
	Target        string            `json:"target"`           // container to stop+recreate
	Image         string            `json:"image"`            // image:tag to run with
	Env           []string          `json:"env,omitempty"`    // expanded env from the running container
	Binds         []string          `json:"binds,omitempty"`  // host_path:container_path[:opts]
	Labels        map[string]string `json:"labels,omitempty"` // includes compose labels for project recognition
	RestartPolicy string            `json:"restart_policy,omitempty"`
	NetworkMode   string            `json:"network_mode,omitempty"`
	ExtraNetworks []string          `json:"extra_networks,omitempty"` // attached after create via `docker network connect`
	PortBindings  []string          `json:"port_bindings,omitempty"`  // pre-formatted "host_ip:host_port:container_port/proto"
}

// handleAgentUpdate dispatches the universal agent.update command for
// docker-bare hosts (FLEET-86). docker+watchtower hosts use the legacy
// watchtower path; systemd-native hosts use FLEET-87's binary-swap.
//
// Flow:
//
//  1. Detect this host's deployment shape so we refuse early on
//     watchtower / systemd-native hosts (those are handled separately).
//  2. Inspect ourselves to capture the running container's spec.
//  3. docker pull the new image.
//  4. Spawn a helper container (same image, --recreate-mode flag) with
//     the captured spec passed in via env var. Helper waits a few
//     seconds then docker-stops + recreates this container — that
//     kills us. Done.
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

	containerName := strings.TrimPrefix(spec.Name, "/")

	// Determine target image. If the admin asked for a pinned tag/digest,
	// use it; otherwise stay on the same repo with :latest.
	targetImage := spec.Config.Image
	if !strings.Contains(targetImage, ":") && !strings.Contains(targetImage, "@") {
		targetImage += ":latest"
	}
	if ps.Target != "" && ps.Target != "latest" {
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

	helperArgs := buildHelperArgs(payload, string(payloadJSON))
	log.Printf("agent.update: spawning recreate helper")
	if out, err := exec.Command("docker", helperArgs...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("spawn helper: %v: %s", err, strings.TrimSpace(string(out)))
	}

	serverURL := strings.TrimRight(os.Getenv("FLEETCOM_URL"), "/")
	token := os.Getenv("FLEETCOM_TOKEN")
	result := json.RawMessage(fmt.Sprintf(`{"phase":"restarting","target_image":%q,"helper_spawned":true}`, targetImage))
	reportResult(serverURL, token, commandResult{
		ID:     id,
		Status: "restarting",
		Result: result,
	})
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

// buildRecreatePayload condenses the inspect output into the helper's
// payload. Always captures full spec — see dockerInspectSpec doc for
// why we don't use compose for the recreate.
func buildRecreatePayload(spec *dockerInspectSpec, containerName, targetImage string) recreatePayload {
	out := recreatePayload{
		Target:        containerName,
		Image:         targetImage,
		Env:           append([]string{}, spec.Config.Env...),
		Binds:         append([]string{}, spec.HostConfig.Binds...),
		Labels:        make(map[string]string, len(spec.Config.Labels)),
		RestartPolicy: spec.HostConfig.RestartPolicy.Name,
		NetworkMode:   spec.HostConfig.NetworkMode,
	}
	for k, v := range spec.Config.Labels {
		out.Labels[k] = v
	}
	// Additional networks beyond NetworkMode get attached post-create.
	for net := range spec.NetworkSettings.Networks {
		if net == out.NetworkMode {
			continue
		}
		out.ExtraNetworks = append(out.ExtraNetworks, net)
	}
	// Port bindings: convert {"8090/tcp":[{"HostIp":"127.0.0.1","HostPort":"8090"}]}
	// into the docker-run "-p" form: "127.0.0.1:8090:8090/tcp".
	for portProto, binds := range spec.HostConfig.PortBindings {
		for _, b := range binds {
			hostIP := b["HostIp"]
			hostPort := b["HostPort"]
			if hostIP == "" {
				out.PortBindings = append(out.PortBindings, hostPort+":"+portProto)
			} else {
				out.PortBindings = append(out.PortBindings, hostIP+":"+hostPort+":"+portProto)
			}
		}
	}
	return out
}

// buildHelperArgs constructs the `docker run` argv that spawns the
// helper container with the captured spec embedded as an env var.
// The helper only needs Docker socket access — no host filesystem
// bind-mounts, since we recreate via plain `docker run` and the daemon
// resolves Source paths against the host fs directly.
func buildHelperArgs(payload recreatePayload, payloadJSON string) []string {
	return []string{
		"run", "--rm", "-d",
		"--label", "fleetcom.role=recreate-helper",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-e", "FLEETCOM_RECREATE_PAYLOAD=" + payloadJSON,
		payload.Image,
		"--recreate-mode",
	}
}

// runRecreateMode is the entrypoint when bosun is invoked with
// --recreate-mode (FLEET-86). Reads the payload from env, waits a few
// seconds for the parent to flush its "restarting" report, then docker-
// stops + recreates the target container via plain `docker run`.
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
		// Not fatal — proceed; rm + run will create fresh. Log so it's
		// visible if there's an underlying issue (e.g. permission).
		log.Printf("recreate-mode: docker stop %s: %v: %s (continuing)", p.Target, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("docker", "rm", p.Target).CombinedOutput(); err != nil {
		log.Printf("recreate-mode: docker rm %s: %v: %s (continuing)", p.Target, err, strings.TrimSpace(string(out)))
	}

	args := []string{"run", "-d", "--name", p.Target}
	if p.NetworkMode != "" && p.NetworkMode != "default" {
		args = append(args, "--network", p.NetworkMode)
	}
	if p.RestartPolicy != "" && p.RestartPolicy != "no" {
		args = append(args, "--restart", p.RestartPolicy)
	}
	for _, e := range p.Env {
		args = append(args, "-e", e)
	}
	for _, b := range p.Binds {
		args = append(args, "-v", b)
	}
	for k, v := range p.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for _, pb := range p.PortBindings {
		args = append(args, "-p", pb)
	}
	args = append(args, p.Image)
	log.Printf("recreate-mode: docker run %s ...", p.Image)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		log.Fatalf("docker run %s: %v: %s", p.Image, err, strings.TrimSpace(string(out)))
	}

	// Attach any extra networks the original container was on. NetworkMode
	// is the primary; multi-attach happens post-create on Docker.
	for _, n := range p.ExtraNetworks {
		if out, err := exec.Command("docker", "network", "connect", n, p.Target).CombinedOutput(); err != nil {
			log.Printf("recreate-mode: network connect %s %s: %v: %s (continuing)", n, p.Target, err, strings.TrimSpace(string(out)))
		}
	}

	log.Printf("recreate-mode: %s recreated successfully", p.Target)
}
