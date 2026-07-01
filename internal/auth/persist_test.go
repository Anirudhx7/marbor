package auth

import (
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func TestSaveLoadStateRoundTrip(t *testing.T) {
	authCfg := config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "team", Key: "sk-team", RateLimit: 100000}},
	}
	mw := NewMiddleware(authCfg)
	// Verify SaveToStore/LoadFromStore with NopStore does not panic.
	st := store.NopStore{}
	if err := mw.SaveToStore(st); err != nil {
		t.Fatalf("SaveToStore: %v", err)
	}
	if err := mw.LoadFromStore(st); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
}
