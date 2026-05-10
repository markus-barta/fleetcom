package db

import "testing"

// FLEET-149 — HostHasAgent powers server-side agent-name isolation on
// /api/bridges/register. Returns true only when the named host has at
// least one heartbeat row for that agent. Used to reject
// out-of-band-deployed bridges that try to register under a name the
// host doesn't actually run.

func TestHostHasAgent_TrueWhenHeartbeatReportedAgent(t *testing.T) {
	store := newTestStore(t)
	agents := []Agent{{Name: "merlin"}, {Name: "nimue"}}
	if _, err := store.UpsertHeartbeat("hsb0", "Debian 12", "6.1", 100, "0.6.5", "docker-bare", "b1", nil, agents, nil); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	for _, name := range []string{"merlin", "nimue"} {
		got, err := store.HostHasAgent("hsb0", name)
		if err != nil {
			t.Fatalf("HostHasAgent(%q): %v", name, err)
		}
		if !got {
			t.Errorf("HostHasAgent(hsb0, %q) = false, want true", name)
		}
	}
}

func TestHostHasAgent_FalseWhenAgentNotReported(t *testing.T) {
	store := newTestStore(t)
	agents := []Agent{{Name: "merlin"}}
	if _, err := store.UpsertHeartbeat("hsb0", "Debian 12", "6.1", 100, "0.6.5", "docker-bare", "b1", nil, agents, nil); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	got, err := store.HostHasAgent("hsb0", "evil")
	if err != nil {
		t.Fatalf("HostHasAgent: %v", err)
	}
	if got {
		t.Errorf("HostHasAgent(hsb0, evil) = true, want false (agent not in heartbeat)")
	}
}

func TestHostHasAgent_FalseForUnknownHost(t *testing.T) {
	store := newTestStore(t)
	got, err := store.HostHasAgent("ghost-host", "merlin")
	if err != nil {
		t.Fatalf("HostHasAgent: %v", err)
	}
	if got {
		t.Errorf("HostHasAgent(ghost-host, merlin) = true, want false")
	}
}

func TestHostHasAgent_HostScoped_DifferentHostsDontShareAgents(t *testing.T) {
	// FLEET-149 cross-host bleed regression. dsc0 reporting "merlin" must
	// not let a bridge on hsb0 register "merlin" — each host's agent set
	// is independent.
	store := newTestStore(t)
	if _, err := store.UpsertHeartbeat("dsc0", "NixOS", "6.6", 100, "0.6.5", "docker-bare", "b1", nil, []Agent{{Name: "merlin"}}, nil); err != nil {
		t.Fatalf("seed dsc0: %v", err)
	}
	if _, err := store.UpsertHeartbeat("hsb0", "Debian", "6.1", 100, "0.6.5", "docker-bare", "b2", nil, []Agent{{Name: "nimue"}}, nil); err != nil {
		t.Fatalf("seed hsb0: %v", err)
	}

	got, err := store.HostHasAgent("hsb0", "merlin")
	if err != nil {
		t.Fatalf("HostHasAgent: %v", err)
	}
	if got {
		t.Errorf("HostHasAgent(hsb0, merlin) = true; merlin lives on dsc0, must not bleed")
	}
}
