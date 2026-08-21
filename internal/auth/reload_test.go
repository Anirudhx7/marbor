package auth

import (
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
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

// TestReloadRecreatesLimiterOnRateLimitChange is the regression guard for
// the bug where Reload() updated existing.rateLimit for an unchanged key but
// left existing.limiter untouched, so a lowered/raised rate_limit had no
// effect on admission until the key's token was rotated - a config change
// that appeared to succeed in the admin UI while the old quota kept being
// enforced.
func TestReloadRecreatesLimiterOnRateLimitChange(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "alice", Key: "sk-alice", RateLimit: 1}},
	})

	mw.mu.RLock()
	ks := mw.byName["alice"]
	mw.mu.RUnlock()

	// Exhaust the 1-request-per-hour bucket.
	if !ks.limiter.allow() {
		t.Fatal("setup: first request should be allowed")
	}
	if ks.limiter.allow() {
		t.Fatal("setup: bucket should be exhausted at rate_limit=1")
	}

	// Reload raising rate_limit for the same key - the limiter must reset,
	// not just the recorded rateLimit field.
	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "alice", Key: "sk-alice", RateLimit: 1000}},
	})

	mw.mu.RLock()
	ks2 := mw.byName["alice"]
	mw.mu.RUnlock()

	if !ks2.limiter.allow() {
		t.Error("raised rate_limit should take effect immediately via a recreated limiter, not require key rotation")
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
