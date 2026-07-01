package auth

import (
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestReloadPreservesCounterForUnchangedKey(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "alice", Key: "sk-alice", RateLimit: 1000}},
	})

	// Burn 3 requests on alice.
	mw.mu.RLock()
	ks := mw.byName["alice"]
	mw.mu.RUnlock()
	ks.counter.increment()
	ks.counter.increment()
	ks.counter.increment()

	today, _, _ := ks.counter.stats()
	if today != 3 {
		t.Fatalf("setup: expected 3 requests, got %d", today)
	}

	// Reload with same key value - counter must survive.
	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "alice", Key: "sk-alice", RateLimit: 500, DailyLimit: 100},
		},
	})

	mw.mu.RLock()
	ks2 := mw.byName["alice"]
	mw.mu.RUnlock()

	today2, _, _ := ks2.counter.stats()
	if today2 != 3 {
		t.Errorf("counter should survive reload: want 3, got %d", today2)
	}
	// Policy field update should take effect.
	if ks2.dailyLimit != 100 {
		t.Errorf("daily limit should update: want 100, got %d", ks2.dailyLimit)
	}
	if ks2.rateLimit != 500 {
		t.Errorf("rate limit should update: want 500, got %d", ks2.rateLimit)
	}
}

func TestReloadRotatedKeyStartsFresh(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "bob", Key: "sk-bob-old", RateLimit: 100}},
	})

	mw.mu.RLock()
	ks := mw.byName["bob"]
	mw.mu.RUnlock()
	ks.counter.increment()
	ks.counter.increment()

	// Reload with ROTATED key value (same name, different token).
	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "bob", Key: "sk-bob-new", RateLimit: 100}},
	})

	mw.mu.RLock()
	ks2 := mw.byName["bob"]
	_, oldKeyExists := mw.keys["sk-bob-old"]
	mw.mu.RUnlock()

	if oldKeyExists {
		t.Error("old token should be removed after rotation")
	}
	today, _, _ := ks2.counter.stats()
	if today != 0 {
		t.Errorf("rotated key should have fresh counter: want 0, got %d", today)
	}
}

func TestReloadDropsRemovedKeys(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "keep", Key: "sk-keep", RateLimit: 100},
			{Name: "drop", Key: "sk-drop", RateLimit: 100},
		},
	})

	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "keep", Key: "sk-keep", RateLimit: 100}},
	})

	mw.mu.RLock()
	_, dropExists := mw.byName["drop"]
	_, keepExists := mw.byName["keep"]
	mw.mu.RUnlock()

	if dropExists {
		t.Error("removed key should not exist after reload")
	}
	if !keepExists {
		t.Error("retained key should still exist after reload")
	}
}

func TestReloadAddsNewKey(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "existing", Key: "sk-existing", RateLimit: 100}},
	})

	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "existing", Key: "sk-existing", RateLimit: 100},
			{Name: "newkey", Key: "sk-new", RateLimit: 200},
		},
	})

	mw.mu.RLock()
	ks, ok := mw.byName["newkey"]
	mw.mu.RUnlock()

	if !ok {
		t.Fatal("new key should be present after reload")
	}
	if ks.createdAt.Before(time.Now().Add(-5 * time.Second)) {
		t.Error("new key should have recent createdAt")
	}
}

func TestReloadTogglesEnabled(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "k", Key: "sk-k", RateLimit: 100}},
	})

	mw.Reload(config.AuthConfig{Enabled: config.BoolPtr(false)})

	mw.mu.RLock()
	enabled := mw.enabled
	mw.mu.RUnlock()

	if enabled {
		t.Error("reload should disable auth when Enabled=false")
	}
}
