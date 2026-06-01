package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type backupState struct {
	interval time.Duration
	lastRun  time.Time
	cached   []BackupPayload
}

func (s *backupState) collect(hostname string, containers []ContainerPayload) []BackupPayload {
	if s == nil {
		return []BackupPayload{}
	}
	if s.interval <= 0 {
		s.interval = 15 * time.Minute
	}
	if !s.lastRun.IsZero() && time.Since(s.lastRun) < s.interval {
		return cloneBackups(s.cached)
	}
	s.cached = detectBackups(hostname, containers)
	s.lastRun = time.Now()
	return cloneBackups(s.cached)
}

func detectBackups(hostname string, containers []ContainerPayload) []BackupPayload {
	out := []BackupPayload{}
	for _, c := range containers {
		if !isResticContainer(c) {
			continue
		}
		b := probeResticContainer(hostname, c)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func isResticContainer(c ContainerPayload) bool {
	hay := strings.ToLower(c.Name + " " + c.Image)
	return strings.Contains(hay, "restic")
}

func probeResticContainer(hostname string, c ContainerPayload) BackupPayload {
	checked := time.Now().UTC().Format(time.RFC3339)
	b := BackupPayload{
		Name:          c.Name,
		Kind:          "restic",
		Status:        "ok",
		ContainerName: c.Name,
		LastCheckedAt: checked,
	}
	if c.State != "running" {
		b.Status = "error"
		b.Error = "restic container is " + c.State
		return b
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// RESTIC_BACKUP_OPTIONS carries the -r repository flag on several hosts.
	// Let the restic container's shell expand its own env; bosun never reads or
	// logs the env file contents.
	script := `if [ -n "${RESTIC_BACKUP_OPTIONS:-}" ]; then exec restic ${RESTIC_BACKUP_OPTIONS} snapshots --json; fi; exec restic snapshots --json`
	out, err := exec.CommandContext(ctx, "docker", "exec", c.Name, "sh", "-c", script).CombinedOutput()
	if err != nil {
		b.Status = "error"
		b.Error = truncateBackupError(fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out))))
		return b
	}

	snapshot, ok, err := latestResticSnapshot(out, hostname)
	if err != nil {
		b.Status = "error"
		b.Error = truncateBackupError(err.Error())
		return b
	}
	if !ok {
		b.Status = "error"
		b.Error = "restic repository has no snapshots"
		return b
	}
	b.LastSuccessAt = snapshot.Time.UTC().Format(time.RFC3339)
	b.SnapshotID = firstNonEmpty(snapshot.ShortID, snapshot.ID)
	b.SnapshotHost = snapshot.Hostname
	b.Paths = snapshot.Paths
	return b
}

type resticSnapshot struct {
	Time     time.Time `json:"time"`
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
}

func latestResticSnapshot(raw []byte, hostname string) (resticSnapshot, bool, error) {
	var snapshots []resticSnapshot
	if err := json.Unmarshal(raw, &snapshots); err != nil {
		return resticSnapshot{}, false, fmt.Errorf("parse restic snapshots json: %w", err)
	}
	var best resticSnapshot
	found := false
	for _, snap := range snapshots {
		if strings.TrimSpace(hostname) != "" && snap.Hostname != "" && snap.Hostname != hostname {
			continue
		}
		if !found || snap.Time.After(best.Time) {
			best = snap
			found = true
		}
	}
	if found {
		return best, true, nil
	}
	for _, snap := range snapshots {
		if !found || snap.Time.After(best.Time) {
			best = snap
			found = true
		}
	}
	return best, found, nil
}

func cloneBackups(in []BackupPayload) []BackupPayload {
	if len(in) == 0 {
		return []BackupPayload{}
	}
	out := make([]BackupPayload, len(in))
	copy(out, in)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateBackupError(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return msg
}
