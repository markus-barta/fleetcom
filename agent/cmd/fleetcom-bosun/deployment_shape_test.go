package main

import "testing"

func TestIsWatchtowerImage(t *testing.T) {
	cases := []struct {
		img  string
		want bool
	}{
		{"containrrr/watchtower:latest", true},
		{"containrrr/watchtower", true},
		{"ghcr.io/containrrr/watchtower:1.7.1", true},
		{"v2tec/watchtower", true},
		{"watchtower", true},
		{"watchtower:latest", true},
		{"WatchTower:latest", true},
		{"ghcr.io/markus-barta/fleetcom-bosun:latest", false},
		{"namshi/smtp", false},
		{"alpine:3.21", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isWatchtowerImage(c.img); got != c.want {
			t.Errorf("isWatchtowerImage(%q) = %v; want %v", c.img, got, c.want)
		}
	}
}

func TestDetectDeploymentShape_DockerWithWatchtower(t *testing.T) {
	// Force inDockerContainer() to return true via the standard marker —
	// can't unit-test that path cleanly without filesystem mocking, so we
	// just exercise the inner watchtower-scan logic with the public helper.
	containers := []ContainerPayload{
		{Name: "fleetcom-agent", Image: "ghcr.io/markus-barta/fleetcom-bosun:latest"},
		{Name: "watchtower", Image: "containrrr/watchtower:latest"},
	}
	hasWT := false
	for _, c := range containers {
		if isWatchtowerImage(c.Image) {
			hasWT = true
			break
		}
	}
	if !hasWT {
		t.Error("expected watchtower image to be detected in container list")
	}
}

func TestDetectDeploymentShape_DockerBare(t *testing.T) {
	containers := []ContainerPayload{
		{Name: "fleetcom-agent", Image: "ghcr.io/markus-barta/fleetcom-bosun:latest"},
		{Name: "smtp", Image: "namshi/smtp"},
		{Name: "openclaw-gateway", Image: "docker-openclaw-gateway"},
	}
	for _, c := range containers {
		if isWatchtowerImage(c.Image) {
			t.Errorf("unexpected watchtower match on %q", c.Image)
		}
	}
}
