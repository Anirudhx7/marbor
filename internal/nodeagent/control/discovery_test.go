package control

import (
	"context"
	"errors"
	"testing"
)

// fakeToolPresence lets a test simulate exactly which tools are present on
// PATH (systemctl/docker/sc/launchctl each check lookPath for their own
// binary name before running anything).
func fakeToolPresence(t *testing.T, present map[string]bool) {
	t.Helper()
	old := lookPath
	lookPath = func(name string) (string, error) {
		if present[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = old })
}

func TestDiscoverPrefersSystemdOverDocker(t *testing.T) {
	fakeToolPresence(t, map[string]bool{"systemctl": true, "docker": true})
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		switch name {
		case "systemctl":
			return "ollama.service loaded active running Ollama\n", nil
		case "docker":
			return "ollama\tollama/ollama:latest\n", nil
		}
		return "", errors.New("unexpected command")
	})

	res := Discover(context.Background(), "ollama", "")
	if res.Driver != "systemd" {
		t.Errorf("Driver = %q, want systemd (higher confidence tier than docker)", res.Driver)
	}
	if res.Identifier != "ollama.service" {
		t.Errorf("Identifier = %q, want ollama.service", res.Identifier)
	}
	if len(res.Evidence) == 0 {
		t.Error("expected non-empty evidence")
	}
}

func TestDiscoverFallsBackToDockerWhenNoSystemdMatch(t *testing.T) {
	fakeToolPresence(t, map[string]bool{"systemctl": true, "docker": true})
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		switch name {
		case "systemctl":
			return "unrelated.service loaded active running Something Else\n", nil
		case "docker":
			return "ollama\tollama/ollama:latest\n", nil
		}
		return "", errors.New("unexpected command")
	})

	res := Discover(context.Background(), "ollama", "")
	if res.Driver != "docker" {
		t.Errorf("Driver = %q, want docker", res.Driver)
	}
	if res.Identifier != "ollama" {
		t.Errorf("Identifier = %q, want ollama", res.Identifier)
	}
}

func TestDiscoverPortProbeNeverSelectsADriver(t *testing.T) {
	fakeToolPresence(t, map[string]bool{})
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "", errors.New("tool not found")
	})

	res := Discover(context.Background(), "ollama", "http://localhost:11434")
	if res.Driver != "" {
		t.Errorf("Driver = %q, want empty - a reachable port must never select a control driver", res.Driver)
	}
	if len(res.Evidence) == 0 {
		t.Error("expected port-reachability evidence to still be recorded")
	}
}

func TestDiscoverNothingFoundAtAll(t *testing.T) {
	fakeToolPresence(t, map[string]bool{})
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "", errors.New("tool not found")
	})

	res := Discover(context.Background(), "ollama", "")
	if res.Driver != "" || res.Identifier != "" || len(res.Evidence) != 0 {
		t.Errorf("expected a fully empty DiscoveryResult, got %+v", res)
	}
}
