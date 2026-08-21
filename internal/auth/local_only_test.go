package auth

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestIsLocalOnly guards the P66 fail-closed policy check: a key configured
// local_only must report true, a normal key must report false, and an
// unknown key name must fail open (false) rather than block an
// unrecognized/anonymous request.
func TestIsLocalOnly(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "finance", Key: "sk-finance", RateLimit: 10, LocalOnly: true},
			{Name: "default", Key: "sk-default", RateLimit: 10},
		},
	})

	if !mw.IsLocalOnly("finance") {
		t.Error("IsLocalOnly(finance) = false, want true")
	}
	if mw.IsLocalOnly("default") {
		t.Error("IsLocalOnly(default) = true, want false")
	}
	if mw.IsLocalOnly("nonexistent") {
		t.Error("IsLocalOnly(nonexistent) = true, want false (fail open for unknown key)")
	}
}

// TestPatchKeyLocalOnly guards that local_only can be toggled after the key
// exists, via the same KeyPatch/PatchKey path as dailyUsdCap/monthlyUsdCap.
func TestPatchKeyLocalOnly(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "ukey", Key: "sk-local", RateLimit: 10},
		},
	})

	if mw.IsLocalOnly("ukey") {
		t.Fatal("pre-patch IsLocalOnly should be false")
	}

	enable := true
	if !mw.PatchKey("ukey", KeyPatch{LocalOnly: &enable}) {
		t.Fatal("PatchKey returned false for existing key")
	}
	if !mw.IsLocalOnly("ukey") {
		t.Error("IsLocalOnly should be true after patching local_only=true")
	}

	disable := false
	if !mw.PatchKey("ukey", KeyPatch{LocalOnly: &disable}) {
		t.Fatal("PatchKey returned false for existing key")
	}
	if mw.IsLocalOnly("ukey") {
		t.Error("IsLocalOnly should be false after patching local_only=false")
	}
}

// TestReloadPreservesLocalOnlyForUnchangedKey guards that a config reload
// (same name, same token) still applies an updated local_only value - the
// same policy-field-update path dailyUsdCap/monthlyUsdCap already use in
// Reload's "same key" branch.
func TestReloadPreservesLocalOnlyForUnchangedKey(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "ukey", Key: "sk-reload", RateLimit: 10},
		},
	})
	if mw.IsLocalOnly("ukey") {
		t.Fatal("pre-reload IsLocalOnly should be false")
	}

	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "ukey", Key: "sk-reload", RateLimit: 10, LocalOnly: true},
		},
	})

	if !mw.IsLocalOnly("ukey") {
		t.Error("IsLocalOnly should be true after reload sets local_only=true for an unchanged-token key")
	}
}

// TestReloadNewKeySetsLocalOnly guards Reload's "new key or rotated token"
// branch: a brand-new key with local_only=true must come up already
// enforcing the policy, not defaulting to false until a later reload.
func TestReloadNewKeySetsLocalOnly(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})

	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "brand-new", Key: "sk-new", RateLimit: 10, LocalOnly: true},
		},
	})

	if !mw.IsLocalOnly("brand-new") {
		t.Error("IsLocalOnly should be true for a brand-new key added via Reload with local_only=true")
	}
}
