package auth

// persist_test.go - usage counters survive a save/load cycle (restart durability).

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestSaveLoadStateRoundTrip(t *testing.T) {
	authCfg := config.AuthConfig{
		Enabled: true,
		Keys:    []config.KeyConfig{{Name: "team", Key: "sk-team", RateLimit: 100000}},
	}
	mw := NewMiddleware(authCfg)
	h := mw.Handler(okHandler())

	// Generate 3 requests and 500 tokens of usage.
	for i := 0; i < 3; i++ {
		if rec := fire(h, "sk-team"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	mw.AddKeyTokens("team", 500)

	path := filepath.Join(t.TempDir(), "usage-state.json")
	if err := mw.SaveState(path); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Simulate a restart: fresh middleware from the same config, then load.
	restarted := NewMiddleware(authCfg)
	if err := restarted.LoadState(path); err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	today, month, tokensMonth, _, _, _, _, ok := restarted.KeyStats("team")
	if !ok {
		t.Fatal("key missing after restore")
	}
	if today != 3 || month != 3 {
		t.Errorf("restored counts today=%d month=%d, want 3/3", today, month)
	}
	if tokensMonth != 500 {
		t.Errorf("restored tokensMonth=%d, want 500", tokensMonth)
	}
}

func TestQuotaSurvivesRestart(t *testing.T) {
	const limit = 2
	authCfg := config.AuthConfig{
		Enabled: true,
		Keys:    []config.KeyConfig{{Name: "q", Key: "sk-q", RateLimit: 100000, DailyLimit: limit}},
	}
	mw := NewMiddleware(authCfg)
	h := mw.Handler(okHandler())

	// Use up the full daily quota.
	for i := 0; i < limit; i++ {
		if rec := fire(h, "sk-q"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}

	path := filepath.Join(t.TempDir(), "state.json")
	if err := mw.SaveState(path); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Restart and reload: the quota must still be exhausted (no bypass).
	restarted := NewMiddleware(authCfg)
	if err := restarted.LoadState(path); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	h2 := restarted.Handler(okHandler())
	if rec := fire(h2, "sk-q"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after restart, over-quota request got %d, want 429 (quota bypassed by restart)", rec.Code)
	}
}

func TestLoadStateMissingFileNoError(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{Enabled: true, Keys: []config.KeyConfig{{Name: "a", Key: "sk-a", RateLimit: 10}}})
	if err := mw.LoadState(filepath.Join(t.TempDir(), "does-not-exist.json")); err != nil {
		t.Errorf("LoadState on missing file should be a no-op, got %v", err)
	}
	if err := mw.SaveState(""); err != nil {
		t.Errorf("SaveState with blank path should be a no-op, got %v", err)
	}
}
