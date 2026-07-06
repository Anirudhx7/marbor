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
	if !cfg.Auth.IsEnabled() {
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

func TestSavingsReferenceRateDefault(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Savings.ReferenceCostPer1K != 0.002 {
		t.Errorf("default reference_cost_per_1k = %v, want 0.002", cfg.Savings.ReferenceCostPer1K)
	}
}

func TestSavingsReferenceRateFromYAML(t *testing.T) {
	yaml := `
savings:
  reference_cost_per_1k: 0.01
`
	tmp, _ := os.CreateTemp("", "config-*.yaml")
	tmp.WriteString(yaml)
	tmp.Close()
	defer os.Remove(tmp.Name())

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Savings.ReferenceCostPer1K != 0.01 {
		t.Errorf("reference_cost_per_1k = %v, want 0.01", cfg.Savings.ReferenceCostPer1K)
	}
}

// TestDuplicateNodeURLNormalized verifies that two nodes with the same
// backend URL are rejected even when they differ only cosmetically (case of
// scheme/host, trailing slash) and are registered under different names —
// e.g. a statically-configured "pve" and an auto-discovered
// "discovered-ollama-1" that both point at the same physical GPU box. Before
// NormalizeNodeURL, Validate() only caught byte-for-byte identical URL
// strings, so this exact real-world case slipped through.
func TestDuplicateNodeURLNormalized(t *testing.T) {
	yaml := `
nodes:
  - name: pve
    url: http://192.168.1.115:11434
  - name: discovered-ollama-1
    url: HTTP://192.168.1.115:11434/
`
	tmp, _ := os.CreateTemp("", "config-*.yaml")
	tmp.WriteString(yaml)
	tmp.Close()
	defer os.Remove(tmp.Name())

	_, err := LoadConfig(tmp.Name())
	if err == nil {
		t.Fatal("expected error for duplicate node URL under different names")
	}
}

func TestNormalizeNodeURL(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // true if a and b should normalize equal
	}{
		{"http://192.168.1.115:11434", "http://192.168.1.115:11434", true},
		{"http://192.168.1.115:11434", "http://192.168.1.115:11434/", true},
		{"HTTP://192.168.1.115:11434", "http://192.168.1.115:11434", true},
		{"http://Host:11434", "http://host:11434", true},
		{"http://192.168.1.115:11434", "http://192.168.1.116:11434", false},
		{"http://192.168.1.115:11434", "https://192.168.1.115:11434", false},
	}
	for _, c := range cases {
		gotA, gotB := NormalizeNodeURL(c.a), NormalizeNodeURL(c.b)
		if (gotA == gotB) != c.want {
			t.Errorf("NormalizeNodeURL(%q)=%q vs NormalizeNodeURL(%q)=%q: equal=%v, want %v",
				c.a, gotA, c.b, gotB, gotA == gotB, c.want)
		}
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
