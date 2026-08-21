package router

import (
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestSetAndGetNodeControlSetting(t *testing.T) {
	r := newTestRouter(nil, config.WebhookConfig{})

	if _, ok := r.NodeControlSetting("gpu-1"); ok {
		t.Fatal("expected no control config before SetNodeControl")
	}

	r.SetNodeControl("gpu-1", ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})
	cfg, ok := r.NodeControlSetting("gpu-1")
	if !ok {
		t.Fatal("expected control config after SetNodeControl")
	}
	if cfg.Driver != "systemd" || cfg.Identifier != "ollama.service" || !cfg.Configured {
		t.Fatalf("NodeControlSetting = %+v, want systemd/ollama.service/configured", cfg)
	}
}

// TestSetNodeControlUnconfiguredRemovesEntry mirrors SetMarborAgent's disable
// behavior: passing Configured:false clears the node from the map entirely,
// so a lifecycle action on it hits the "no control driver configured" path
// rather than a stale accepted value.
func TestSetNodeControlUnconfiguredRemovesEntry(t *testing.T) {
	r := newTestRouter(nil, config.WebhookConfig{})

	r.SetNodeControl("gpu-1", ControlConfig{Driver: "docker", Identifier: "ollama", Configured: true})
	if _, ok := r.NodeControlSetting("gpu-1"); !ok {
		t.Fatal("expected control config to be set")
	}

	r.SetNodeControl("gpu-1", ControlConfig{Configured: false})
	if _, ok := r.NodeControlSetting("gpu-1"); ok {
		t.Fatal("expected control config to be removed after Configured:false")
	}
}
