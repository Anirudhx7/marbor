package config

import "testing"

// TestCloudProvidersAbsentByDefault verifies that a minimal config with only
// nodes defined has no cloud providers at all. This is the trust guarantee:
// nothing goes to OpenAI/Anthropic unless the operator explicitly configures it.
func TestCloudProvidersAbsentByDefault(t *testing.T) {
	cfg := Config{Nodes: []NodeConfig{{Name: "local-gpu", URL: "http://localhost:11434"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(cfg.CloudProviders) != 0 {
		t.Errorf("cloud_providers: got %d entries, want 0 - cloud overflow must be off by default", len(cfg.CloudProviders))
		for i, cp := range cfg.CloudProviders {
			t.Logf("  provider[%d]: name=%q provider=%q enabled=%v", i, cp.Name, cp.Provider, cp.Enabled)
		}
	}
}

// TestEmptyConfigHasNoCloudProviders verifies that even a completely empty
// config (no sections at all) results in zero cloud providers after Validate().
func TestEmptyConfigHasNoCloudProviders(t *testing.T) {
	var cfg Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate empty config: %v", err)
	}
	if len(cfg.CloudProviders) != 0 {
		t.Errorf("empty config cloud_providers: got %d, want 0", len(cfg.CloudProviders))
	}
}

// TestCloudProviderDisabledFlagRespected verifies that a config listing a
// provider with enabled:false loads it as disabled. This is the control: the
// operator can declare a provider without activating it.
func TestCloudProviderDisabledFlagRespected(t *testing.T) {
	cfg := Config{
		Nodes: []NodeConfig{{Name: "local-gpu", URL: "http://localhost:11434"}},
		CloudProviders: []CloudProvider{
			{Name: "openai-staging", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-placeholder", Enabled: false},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(cfg.CloudProviders) != 1 {
		t.Fatalf("expected 1 cloud provider, got %d", len(cfg.CloudProviders))
	}
	if cfg.CloudProviders[0].Enabled {
		t.Errorf("provider with enabled:false loaded as enabled=true - cloud overflow must respect the flag")
	}
}

// TestCloudProviderEnabledLoadsCorrectly is the positive control: a fully
// specified provider with enabled:true must load as enabled.
func TestCloudProviderEnabledLoadsCorrectly(t *testing.T) {
	cfg := Config{
		Nodes: []NodeConfig{{Name: "local-gpu", URL: "http://localhost:11434"}},
		CloudProviders: []CloudProvider{
			{Name: "openai-prod", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-real-key", Enabled: true},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(cfg.CloudProviders) != 1 {
		t.Fatalf("expected 1 cloud provider, got %d", len(cfg.CloudProviders))
	}
	cp := cfg.CloudProviders[0]
	if !cp.Enabled {
		t.Errorf("provider with enabled:true loaded as enabled=false")
	}
	if cp.Name != "openai-prod" {
		t.Errorf("provider name = %q, want openai-prod", cp.Name)
	}
	if cp.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("base_url = %q, want https://api.openai.com/v1", cp.BaseURL)
	}
}

// TestCloudProviderEnabledWithoutCredentialsIsRejected verifies that Validate()
// rejects a provider that is enabled but has no base_url or api_key. This
// prevents a misconfigured operator from accidentally enabling cloud routing
// with incomplete credentials.
func TestCloudProviderEnabledWithoutCredentialsIsRejected(t *testing.T) {
	cfg := Config{
		Nodes:          []NodeConfig{{Name: "local-gpu", URL: "http://localhost:11434"}},
		CloudProviders: []CloudProvider{{Name: "broken", Provider: "openai", Enabled: true}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for enabled provider without base_url/api_key, got nil")
	}
}
