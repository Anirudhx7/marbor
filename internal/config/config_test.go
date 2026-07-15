package config

import "testing"

func TestValidateAppliesDefaultsAndKeepsExplicitValues(t *testing.T) {
	cfg := Config{
		Proxy: ProxyConfig{Port: 11434},
		Auth: AuthConfig{
			Enabled: BoolPtr(true),
			Keys: []KeyConfig{
				{Name: "test", Key: "sk-test", RateLimit: 100, Models: []string{"llama3.2:8b"}},
			},
		},
		Nodes: []NodeConfig{
			{Name: "gpu-0", URL: "http://localhost:11435", GPUModel: "NVIDIA RTX 4090"},
		},
		Routing: RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, Fallback: "least-connections"},
		Metrics: MetricsConfig{Enabled: true, Port: 9090},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
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
	cfg := Config{
		Nodes: []NodeConfig{{Name: "a", URL: "http://localhost:1"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
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

func TestSavingsReferenceRateExplicitValue(t *testing.T) {
	cfg := Config{Savings: SavingsConfig{ReferenceCostPer1K: 0.01}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Savings.ReferenceCostPer1K != 0.01 {
		t.Errorf("reference_cost_per_1k = %v, want 0.01", cfg.Savings.ReferenceCostPer1K)
	}
}

// TestDuplicateNodeURLNormalized verifies that two nodes with the same
// backend URL are rejected even when they differ only cosmetically (case of
// scheme/host, trailing slash) and are registered under different names --
// e.g. a statically-configured "pve" and an auto-discovered
// "discovered-ollama-1" that both point at the same physical GPU box. Before
// NormalizeNodeURL, Validate() only caught byte-for-byte identical URL
// strings, so this exact real-world case slipped through.
func TestDuplicateNodeURLNormalized(t *testing.T) {
	cfg := Config{
		Nodes: []NodeConfig{
			{Name: "pve", URL: "http://192.168.1.115:11434"},
			{Name: "discovered-ollama-1", URL: "HTTP://192.168.1.115:11434/"},
		},
	}
	if err := cfg.Validate(); err == nil {
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

// TestAuditRetentionDaysValidate verifies 0 (the admin's explicit "keep
// audit log entries forever" choice) survives Validate() unchanged, while a
// negative value is rejected. Regression for a version of this check that
// silently coerced 0 back to a 30-day default, making "forever" impossible
// for an admin to actually choose.
func TestAuditRetentionDaysValidate(t *testing.T) {
	cfg := Config{Audit: AuditConfig{RetentionDays: 0}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with RetentionDays=0: %v", err)
	}
	if cfg.Audit.RetentionDays != 0 {
		t.Fatalf("RetentionDays=0 must be preserved as 'forever', got %d", cfg.Audit.RetentionDays)
	}

	cfg = Config{Audit: AuditConfig{RetentionDays: 90}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with RetentionDays=90: %v", err)
	}
	if cfg.Audit.RetentionDays != 90 {
		t.Fatalf("RetentionDays=90 must be preserved, got %d", cfg.Audit.RetentionDays)
	}

	cfg = Config{Audit: AuditConfig{RetentionDays: -1}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() with RetentionDays=-1 should have failed")
	}
}

func TestDuplicateKeyName(t *testing.T) {
	cfg := Config{
		Auth: AuthConfig{
			Enabled: BoolPtr(true),
			Keys: []KeyConfig{
				{Name: "dup", Key: "sk-1"},
				{Name: "dup", Key: "sk-2"},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for duplicate key name")
	}
}
