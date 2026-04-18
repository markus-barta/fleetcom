package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version info — injected at build time via ldflags.
var (
	Version   = "0.1.0"
	BuildTime = "unknown"
)

// HeartbeatPayload is the full state snapshot sent every interval.
// HwStatic / HwLive / Fastfetch are optional and only included when bosun
// has fresh values — see sendHeartbeat for the cadence rules.
type HeartbeatPayload struct {
	Hostname      string             `json:"hostname"`
	OS            string             `json:"os"`
	Kernel        string             `json:"kernel"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	AgentVersion  string             `json:"agent_version"`
	Containers    []ContainerPayload `json:"containers"`
	Agents        []AgentPayload     `json:"agents"`
	HwStatic      *HwStatic          `json:"hw_static,omitempty"`
	HwLive        *HwLive            `json:"hw_live,omitempty"`
	Fastfetch     json.RawMessage    `json:"fastfetch_json,omitempty"`
}

type ContainerPayload struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	State        string `json:"state"`
	Health       string `json:"health"`
	RestartCount int    `json:"restart_count"`
	StartedAt    string `json:"started_at"`
	ExitCode     int    `json:"exit_code"`
	OOMKilled    bool   `json:"oom_killed"`
}

type AgentPayload struct {
	Name      string `json:"name"`
	AgentType string `json:"agent_type"`
	Status    string `json:"status"`
}

// ContainerEventPayload is sent in real-time when a container lifecycle event fires.
type ContainerEventPayload struct {
	Hostname  string `json:"hostname"`
	Event     string `json:"event"`
	Container string `json:"container"`
	Image     string `json:"image"`
	ExitCode  int    `json:"exit_code"`
	OOMKilled bool   `json:"oom_killed"`
	Timestamp string `json:"timestamp"`
}

func main() {
	serverURL := os.Getenv("FLEETCOM_URL")
	if serverURL == "" {
		log.Fatal("FLEETCOM_URL is required")
	}
	serverURL = strings.TrimRight(serverURL, "/")

	token := os.Getenv("FLEETCOM_TOKEN")
	if token == "" {
		log.Fatal("FLEETCOM_TOKEN is required")
	}

	agentsJSON := os.Getenv("FLEETCOM_AGENTS")
	var agents []AgentPayload
	if agentsJSON != "" {
		if err := json.Unmarshal([]byte(agentsJSON), &agents); err != nil {
			log.Printf("warning: cannot parse FLEETCOM_AGENTS: %v", err)
		}
	}

	hostname := getHostname()
	interval := 60 * time.Second
	agentVersionStr := formatAgentVersion()

	log.Printf("FleetCom Bosun %s starting: host=%s server=%s interval=%s", agentVersionStr, hostname, serverURL, interval)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		cancel()
	}()

	// Start Docker event watcher if socket is available
	socketPath := dockerSocketPath()
	if socketPath != "" {
		log.Printf("Docker socket found at %s — starting event watcher", socketPath)
		go watchDockerEvents(ctx, socketPath, serverURL, token, hostname)
	} else {
		log.Println("no Docker socket found — heartbeat-only mode")
	}

	// Hardware/metadata state: track last static hash + last fastfetch time
	// so we only send Static on change and refresh fastfetch once a day.
	hw := &hwState{fastfetchInterval: 24 * time.Hour}

	// Periodic heartbeat
	sendHeartbeat(serverURL, token, hostname, socketPath, agents, agentVersionStr, &interval, hw)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendHeartbeat(serverURL, token, hostname, socketPath, agents, agentVersionStr, &interval, hw)
			ticker.Reset(interval)
		}
	}
}

// hwState tracks what bosun has already sent so we avoid repeating the
// large static block every 60s. cachedCPUCores is updated each static
// scan and used by collectLive() to pre-compute cpu_used_pct.
type hwState struct {
	lastStaticHash    string
	cachedCPUCores    int
	lastFastfetchRun  time.Time
	fastfetchInterval time.Duration
}

func dockerSocketPath() string {
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		return "/var/run/docker.sock"
	}
	return ""
}

func formatAgentVersion() string {
	// Format: "0.1.0 (2026-04-12, 17:45:27)" or just "0.1.0" if no build time
	if BuildTime != "" && BuildTime != "unknown" {
		// Parse ISO8601 and reformat
		if t, err := time.Parse(time.RFC3339, BuildTime); err == nil {
			return fmt.Sprintf("%s (%s, %s)", Version, t.Format("2006-01-02"), t.Format("15:04:05"))
		}
		// Try simpler format
		if t, err := time.Parse("2006-01-02T15:04:05Z", BuildTime); err == nil {
			return fmt.Sprintf("%s (%s, %s)", Version, t.Format("2006-01-02"), t.Format("15:04:05"))
		}
	}
	return Version
}

func sendHeartbeat(serverURL, token, hostname, socketPath string, agents []AgentPayload, agentVersion string, interval *time.Duration, hw *hwState) {
	containers := listContainers(socketPath)

	payload := HeartbeatPayload{
		Hostname:      hostname,
		OS:            getOS(),
		Kernel:        getKernel(),
		UptimeSeconds: getUptime(),
		AgentVersion:  agentVersion,
		Containers:    containers,
		Agents:        agents,
	}

	// Static first so we have a fresh core count for live's CPU %. Only
	// sent on first beat or when something changed (e.g., new mount).
	if hw != nil {
		static := collectStatic()
		if static.CPUCores > 0 {
			hw.cachedCPUCores = static.CPUCores
		}
		if h := hwStaticHash(static); h != "" && h != hw.lastStaticHash {
			payload.HwStatic = &static
			hw.lastStaticHash = h
		}
	}

	// Live block every beat (cheap — handful of /proc + /sys reads).
	cores := 0
	if hw != nil {
		cores = hw.cachedCPUCores
	}
	live := collectLive(cores)
	payload.HwLive = &live

	if hw != nil {

		// Fastfetch: run on first beat, then every fastfetchInterval.
		if hw.lastFastfetchRun.IsZero() || time.Since(hw.lastFastfetchRun) >= hw.fastfetchInterval {
			if raw := runFastfetch(8 * time.Second); len(raw) > 0 {
				payload.Fastfetch = raw
			}
			// Always stamp, even on failure — avoids hammering a broken binary.
			hw.lastFastfetchRun = time.Now()
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("heartbeat marshal error: %v", err)
		return
	}

	resp, err := doPost(serverURL+"/api/heartbeat", token, data)
	if err != nil {
		log.Printf("heartbeat error: %v", err)
		return
	}

	// Read server-provided interval
	var result struct {
		OK       bool `json:"ok"`
		Interval int  `json:"interval"`
	}
	if err := json.Unmarshal(resp, &result); err == nil && result.Interval >= 10 {
		newInterval := time.Duration(result.Interval) * time.Second
		if newInterval != *interval {
			log.Printf("interval updated: %s → %s", *interval, newInterval)
			*interval = newInterval
		}
	}

	log.Printf("heartbeat sent: %d containers, %d agents", len(containers), len(agents))
}

func doPost(url, token string, body []byte) ([]byte, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)

	if resp.StatusCode >= 400 {
		return buf.Bytes(), fmt.Errorf("HTTP %d: %s", resp.StatusCode, buf.String())
	}
	return buf.Bytes(), nil
}

// ---------- Docker socket communication ----------

// dockerHTTPClient creates an HTTP client that talks over the Unix socket.
func dockerHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// dockerStreamClient is like dockerHTTPClient but without a timeout (for event streaming).
func dockerStreamClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
}

// listContainers calls GET /containers/json?all=true via the Docker socket.
func listContainers(socketPath string) []ContainerPayload {
	if socketPath == "" {
		return []ContainerPayload{}
	}

	client := dockerHTTPClient(socketPath)
	resp, err := client.Get("http://docker/containers/json?all=true")
	if err != nil {
		log.Printf("docker list error: %v", err)
		return []ContainerPayload{}
	}
	defer resp.Body.Close()

	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
		Image string   `json:"Image"`
		State string   `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		log.Printf("docker list decode error: %v", err)
		return []ContainerPayload{}
	}

	// Now inspect each container for detailed health info
	out := make([]ContainerPayload, 0, len(containers))
	for _, c := range containers {
		name := c.Names[0]
		if strings.HasPrefix(name, "/") {
			name = name[1:]
		}

		cp := ContainerPayload{
			Name:  name,
			Image: c.Image,
			State: c.State,
		}

		// Inspect for health details
		if info := inspectContainer(socketPath, c.ID); info != nil {
			cp.Health = info.Health
			cp.RestartCount = info.RestartCount
			cp.StartedAt = info.StartedAt
			cp.ExitCode = info.ExitCode
			cp.OOMKilled = info.OOMKilled
		}

		out = append(out, cp)
	}
	return out
}

type inspectInfo struct {
	Health       string
	RestartCount int
	StartedAt    string
	ExitCode     int
	OOMKilled    bool
}

func inspectContainer(socketPath, id string) *inspectInfo {
	client := dockerHTTPClient(socketPath)
	resp, err := client.Get("http://docker/containers/" + id + "/json")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		RestartCount int `json:"RestartCount"`
		State        struct {
			Status    string `json:"Status"`
			ExitCode  int    `json:"ExitCode"`
			OOMKilled bool   `json:"OOMKilled"`
			StartedAt string `json:"StartedAt"`
			Health    *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	info := &inspectInfo{
		RestartCount: result.RestartCount,
		StartedAt:    result.State.StartedAt,
		ExitCode:     result.State.ExitCode,
		OOMKilled:    result.State.OOMKilled,
	}
	if result.State.Health != nil {
		info.Health = result.State.Health.Status
	}
	return info
}

// watchDockerEvents subscribes to the Docker events stream and sends events to the server.
func watchDockerEvents(ctx context.Context, socketPath, serverURL, token, hostname string) {
	filters := `{"event":["die","start","restart","oom","health_status"],"type":["container"]}`
	url := fmt.Sprintf("http://docker/events?filters=%s", filters)

	for {
		if err := streamEvents(ctx, socketPath, url, serverURL, token, hostname); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("docker event stream error: %v — reconnecting in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func streamEvents(ctx context.Context, socketPath, url, serverURL, token, hostname string) error {
	client := dockerStreamClient(socketPath)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	log.Println("docker event stream connected")

	decoder := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Status string `json:"status"`
			ID     string `json:"id"`
			From   string `json:"from"`
			Type   string `json:"Type"`
			Action string `json:"Action"`
			Actor  struct {
				ID         string            `json:"ID"`
				Attributes map[string]string `json:"Attributes"`
			} `json:"Actor"`
			Time     int64 `json:"time"`
			TimeNano int64 `json:"timeNano"`
		}

		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode event: %w", err)
		}

		containerName := event.Actor.Attributes["name"]
		image := event.Actor.Attributes["image"]
		ts := time.Unix(event.Time, 0).UTC().Format(time.RFC3339)

		// Map Docker event action to our event type
		eventType := event.Action
		switch {
		case strings.HasPrefix(event.Action, "health_status"):
			eventType = "health_status"
		}

		log.Printf("docker event: %s %s (%s)", eventType, containerName, image)

		// Get exit code and OOM status from inspect for die events
		var exitCode int
		var oomKilled bool
		if eventType == "die" {
			if code, err := strconv.Atoi(event.Actor.Attributes["exitCode"]); err == nil {
				exitCode = code
			}
			// Check if it was an OOM kill
			if info := inspectContainer(socketPath, event.Actor.ID); info != nil {
				oomKilled = info.OOMKilled
			}
		}

		payload := ContainerEventPayload{
			Hostname:  hostname,
			Event:     eventType,
			Container: containerName,
			Image:     image,
			ExitCode:  exitCode,
			OOMKilled: oomKilled,
			Timestamp: ts,
		}

		data, err := json.Marshal(payload)
		if err != nil {
			log.Printf("event marshal error: %v", err)
			continue
		}

		if _, err := doPost(serverURL+"/api/container-events", token, data); err != nil {
			log.Printf("event post error: %v", err)
		}
	}
}

// ---------- System info collection ----------

func getHostname() string {
	// Try reading from env first (container might override)
	if h := os.Getenv("FLEETCOM_HOSTNAME"); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

func getOS() string {
	// Try /host/etc/os-release first (mounted from host)
	for _, path := range []string{"/host/etc/os-release", "/etc/os-release"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				val := strings.TrimPrefix(line, "PRETTY_NAME=")
				val = strings.Trim(val, "\"")
				return val
			}
		}
	}
	return "unknown"
}

func getKernel() string {
	// Try /host/proc/version first
	for _, path := range []string{"/host/proc/version", "/proc/version"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parts := strings.Fields(string(data))
		if len(parts) >= 3 {
			return parts[2]
		}
	}
	return "unknown"
}

func getUptime() int64 {
	// Try /host/proc/uptime first
	for _, path := range []string{"/host/proc/uptime", "/proc/uptime"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parts := strings.Fields(string(data))
		if len(parts) >= 1 {
			if secs, err := strconv.ParseFloat(parts[0], 64); err == nil {
				return int64(secs)
			}
		}
	}
	return 0
}
