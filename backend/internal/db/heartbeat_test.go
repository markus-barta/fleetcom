package db

import (
	"testing"
)

// FLEET-124 — UpsertHeartbeat must preserve a previously-known
// agent_version when the incoming heartbeat omits it. dsc0 hit this
// when a stale NixOS fleetcom-agent.timer raced bosun and clobbered
// the version field every other heartbeat, flipping the dashboard
// LEGACY badge on/off.

func TestUpsertHeartbeat_AgentVersion_EmptyPreservesPrevious(t *testing.T) {
	store := newTestStore(t)

	if _, err := store.UpsertHeartbeat("dsc0", "NixOS 25.05", "6.6.0", 100, "0.6.0 (2026-05-02, 15:14:22)", "docker+watchtower", "boot-1", nil, nil, nil); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	if _, err := store.UpsertHeartbeat("dsc0", "NixOS 25.05", "6.6.0", 200, "", "", "", nil, nil, nil); err != nil {
		t.Fatalf("legacy heartbeat: %v", err)
	}

	got, err := store.AgentVersionForHost("dsc0")
	if err != nil {
		t.Fatalf("AgentVersionForHost: %v", err)
	}
	if want := "0.6.0 (2026-05-02, 15:14:22)"; got != want {
		t.Errorf("agent_version after empty heartbeat: got %q, want %q (legacy heartbeat must not clobber a known version)", got, want)
	}
}

func TestUpsertHeartbeat_AgentVersion_NonEmptyOverwrites(t *testing.T) {
	store := newTestStore(t)

	if _, err := store.UpsertHeartbeat("dsc0", "NixOS 25.05", "6.6.0", 100, "0.6.0 (2026-05-02, 15:14:22)", "docker+watchtower", "boot-1", nil, nil, nil); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	if _, err := store.UpsertHeartbeat("dsc0", "NixOS 25.05", "6.6.0", 200, "0.6.1 (2026-05-04, 10:00:00)", "docker+watchtower", "boot-1", nil, nil, nil); err != nil {
		t.Fatalf("upgrade heartbeat: %v", err)
	}

	got, err := store.AgentVersionForHost("dsc0")
	if err != nil {
		t.Fatalf("AgentVersionForHost: %v", err)
	}
	if want := "0.6.1 (2026-05-04, 10:00:00)"; got != want {
		t.Errorf("agent_version after upgrade: got %q, want %q", got, want)
	}
}
