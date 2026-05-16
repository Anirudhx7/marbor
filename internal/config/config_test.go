package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	yaml := `
proxy:
  port: 11434
  log_level: debug
auth:
  enabled: true
  keys:
    - name: test
      key: sk-test
      rate_limit: 100
      models:
        - llama3.2:8b
nodes:
  - name: gpu-0
    url: http://localhost:11435
    gpu_model: NVIDIA RTX 4090
routing:
  strategy: warm-first
  poll_interval_ms: 2000
  fallback: least-connections
metrics:
  enabled: true
  port: 9090
`
	tmp, _ := os.CreateTemp("", "config-*.yaml")
	tmp.WriteString(yaml)
	tmp.Close()
	defer os.Remove(tmp.Name())

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Proxy.Port != 11434 {
		t.Errorf("port = %d, want 11434", cfg.Proxy.Port)
	}
	if !cfg.Auth.Enabled {
		t.Error("auth should be enabled")
	}
	if len(cfg.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(cfg.Nodes))
	}
	if cfg.Nodes[0].GPUModel != "NVIDIA RTX 4090" {
		t.Errorf("gpu_model = %q, want NVIDIA RTX 4090", cfg.Nodes[0].GPUModel)
	}
}

func TestDefaults(t *testing.T) {
	yaml := `
nodes:
  - name: a
    url: http://localhost:1
`
	tmp, _ := os.CreateTemp("", "config-*.yaml")
	tmp.WriteString(yaml)
	tmp.Close()
	defer os.Remove(tmp.Name())

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Proxy.Port != 11434 {
		t.Errorf("default port = %d, want 11434", cfg.Proxy.Port)
	}
	if cfg.Routing.PollIntervalMs != 2000 {
		t.Errorf("default poll = %d, want 2000", cfg.Routing.PollIntervalMs)
	}
}

func TestDuplicateKeyName(t *testing.T) {
	yaml := `
auth:
  enabled: true
  keys:
    - name: dup
      key: sk-1
    - name: dup
      key: sk-2
`
	tmp, _ := os.CreateTemp("", "config-*.yaml")
	tmp.WriteString(yaml)
	tmp.Close()
	defer os.Remove(tmp.Name())

	_, err := LoadConfig(tmp.Name())
	if err == nil {
		t.Fatal("expected error for duplicate key name")
	}
}
