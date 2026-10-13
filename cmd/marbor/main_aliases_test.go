package main

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// TestBootDropsInvalidModelAliases seeds the settings table with a mix of
// valid and invalid aliases and asserts that the boot overlay keeps the valid
// ones, drops the rest, and leaves a config that passes the fatal
// post-overlay validation - one bad stored row must never block startup.
func TestBootDropsInvalidModelAliases(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := store.SetJSONSetting(st, "routing_model_aliases", map[string]string{
		"gpt-4":     "llama3.2:8b",
		"self":      "self",
		"a":         "b",
		"b":         "c",
		"bad\nname": "llama3.2:8b",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg := &config.Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults Validate: %v", err)
	}
	applyPersistedSettings(cfg, st)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("post-overlay Validate must succeed after sanitizing aliases, got %v", err)
	}
	want := map[string]string{"gpt-4": "llama3.2:8b", "b": "c"}
	if len(cfg.Routing.ModelAliases) != len(want) {
		t.Fatalf("aliases after boot = %v, want %v", cfg.Routing.ModelAliases, want)
	}
	for k, v := range want {
		if cfg.Routing.ModelAliases[k] != v {
			t.Fatalf("aliases[%q] = %q, want %q", k, cfg.Routing.ModelAliases[k], v)
		}
	}

	r := router.New(cfg.Routing, nil, nil)
	if got, ok := r.ResolveModelAlias("gpt-4"); !ok || got != "llama3.2:8b" {
		t.Fatalf("router did not pick up boot aliases: %q, %v", got, ok)
	}
}
