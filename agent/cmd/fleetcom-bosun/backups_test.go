package main

import (
	"testing"
)

func TestIsResticContainer(t *testing.T) {
	cases := []struct {
		name string
		c    ContainerPayload
		want bool
	}{
		{"name", ContainerPayload{Name: "restic-cron-hetzner", Image: "local"}, true},
		{"compose", ContainerPayload{Name: "csb1-restic-cron-hetzner-1", Image: "local"}, true},
		{"image", ContainerPayload{Name: "backup", Image: "restic/restic:latest"}, true},
		{"other", ContainerPayload{Name: "watchtower", Image: "containrrr/watchtower"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResticContainer(tc.c); got != tc.want {
				t.Fatalf("isResticContainer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLatestResticSnapshotPrefersCurrentHost(t *testing.T) {
	raw := []byte(`[
		{"time":"2026-05-30T01:30:00Z","short_id":"old-hsb1","hostname":"hsb1","paths":["/backup/home"]},
		{"time":"2026-06-01T01:30:00Z","short_id":"new-csb1","hostname":"csb1","paths":["/backup/etc"]},
		{"time":"2026-05-31T01:30:00Z","short_id":"new-hsb1","hostname":"hsb1","paths":["/backup/etc"]}
	]`)
	snap, ok, err := latestResticSnapshot(raw, "hsb1")
	if err != nil {
		t.Fatalf("latestResticSnapshot: %v", err)
	}
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.ShortID != "new-hsb1" {
		t.Fatalf("snapshot = %q, want new-hsb1", snap.ShortID)
	}
}

func TestLatestResticSnapshotFallsBackWhenHostAbsent(t *testing.T) {
	raw := []byte(`[
		{"time":"2026-05-30T01:30:00Z","short_id":"old","hostname":"csb0"},
		{"time":"2026-06-01T01:30:00Z","short_id":"new","hostname":"csb1"}
	]`)
	snap, ok, err := latestResticSnapshot(raw, "hsb8")
	if err != nil {
		t.Fatalf("latestResticSnapshot: %v", err)
	}
	if !ok || snap.ShortID != "new" {
		t.Fatalf("snapshot = %#v ok=%v, want fallback new", snap, ok)
	}
}
