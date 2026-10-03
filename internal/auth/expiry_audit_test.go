package auth

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// eventRecorder collects audit hook calls.
type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) hook(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, name)
}

func (r *eventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func authCfg(keys ...config.KeyConfig) config.AuthConfig {
	return config.AuthConfig{Enabled: config.BoolPtr(true), Keys: keys}
}

func keyWith(name, secret, expiresAt string) config.KeyConfig {
	return config.KeyConfig{Name: name, Key: secret, RateLimit: 1000, ExpiresAt: expiresAt}
}

func TestMalformedExpiryAuditedAtInitialLoad(t *testing.T) {
	mw := NewMiddleware(authCfg(
		keyWith("bad", "sk-bad", "next tuesday"),
		keyWith("ok", "sk-ok", "2099-01-01"),
		keyWith("none", "sk-none", ""),
	))
	var rec eventRecorder
	mw.SetExpiryAuditHook(rec.hook)
	got := rec.snapshot()
	if len(got) != 1 || got[0] != "bad" {
		t.Fatalf("events = %v, want exactly [bad]", got)
	}
}

func TestMalformedExpiryAuditedAfterReload(t *testing.T) {
	mw := NewMiddleware(authCfg(keyWith("k", "sk-k", "2099-01-01")))
	var rec eventRecorder
	mw.SetExpiryAuditHook(rec.hook)
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("valid key emitted %d events", n)
	}

	mw.Reload(authCfg(keyWith("k", "sk-k", "garbage-1")))
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("after first malformed reload: %d events, want 1", n)
	}
	mw.Reload(authCfg(keyWith("k", "sk-k", "garbage-1")))
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("same malformed value reloaded: %d events, want still 1", n)
	}
	mw.Reload(authCfg(keyWith("k", "sk-k", "garbage-2")))
	got := rec.snapshot()
	if len(got) != 2 || got[1] != "k" {
		t.Fatalf("different malformed value: events = %v, want 2 events for k", got)
	}
}

func TestMalformedExpiryHookDedupesPerKeyAndValue(t *testing.T) {
	mw := NewMiddleware(authCfg())
	var rec eventRecorder
	mw.SetExpiryAuditHook(rec.hook)

	mw.AddKey(keyWith("a", "sk-a", "bad"))
	mw.AddKey(keyWith("a", "sk-a", "bad"))  // same key, same value
	mw.AddKey(keyWith("b", "sk-b", "bad"))  // different key, same value
	mw.AddKey(keyWith("a", "sk-a", "bad2")) // same key, new value
	mw.SetExpiryAuditHook(rec.hook)         // re-setting must not replay already reported pairs

	got := rec.snapshot()
	want := []string{"a", "b", "a"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}

	// PatchKey introducing a new malformed value is reported once as well.
	v := "bad3"
	mw.PatchKey("b", KeyPatch{ExpiresAt: &v})
	mw.PatchKey("b", KeyPatch{ExpiresAt: &v})
	if n := len(rec.snapshot()); n != 4 {
		t.Fatalf("after PatchKey: %d events, want 4", n)
	}
}

func TestMalformedExpiryRequestReturns401(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mw := NewMiddleware(authCfg(keyWith("bad", "sk-secret-value", "next tuesday")))
	var rec eventRecorder
	mw.SetExpiryAuditHook(rec.hook)
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer sk-secret-value")
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401", i, rec2.Code)
		}
		if !strings.Contains(rec2.Body.String(), "api_key_expired") {
			t.Fatalf("request %d: body %q lacks api_key_expired", i, rec2.Body.String())
		}
	}
	// Only the one load-time event; requests add none.
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("events after 5 requests = %d, want 1 (none per request)", n)
	}
}

func TestMalformedExpiryAuditEventNamesKeyNotValue(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mw := NewMiddleware(authCfg(keyWith("bad", "sk-very-secret", "RAW-MALFORMED-VALUE")))
	var rec eventRecorder
	mw.SetExpiryAuditHook(rec.hook)
	mw.Reload(authCfg(keyWith("bad", "sk-very-secret", "RAW-MALFORMED-VALUE-2")))

	for _, ev := range rec.snapshot() {
		if ev != "bad" {
			t.Errorf("event %q is not just the key name", ev)
		}
	}
	for _, s := range []string{ExpiryAuditAction, ExpiryAuditDetails, buf.String()} {
		if strings.Contains(s, "sk-very-secret") || strings.Contains(s, "RAW-MALFORMED") {
			t.Errorf("leaked secret or raw value in %q", s)
		}
	}
}

func TestMalformedExpiryHookNotCalledUnderLock(t *testing.T) {
	mw := NewMiddleware(authCfg(keyWith("bad", "sk-bad", "oops")))
	done := make(chan struct{})
	go func() {
		defer close(done)
		var calls int
		mw.SetExpiryAuditHook(func(name string) {
			calls++
			// Each of these needs a Middleware lock; a held lock would deadlock.
			ExpiryStatus("oops", time.Now())
			mw.KeyStats(name)
			mw.AddKey(keyWith("other", "sk-other", ""))
			mw.PatchKey("other", KeyPatch{})
			mw.Reload(authCfg(keyWith("bad", "sk-bad", "oops"), keyWith("other", "sk-other", "")))
		})
		mw.Reload(authCfg(keyWith("bad2", "sk-bad2", "oops2")))
		mw.AddKey(keyWith("bad3", "sk-bad3", "oops3"))
		v := "oops4"
		mw.PatchKey("bad3", KeyPatch{ExpiresAt: &v})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: hook was called while a Middleware lock was held")
	}
}

func TestMalformedExpiryNilHookIsSafe(t *testing.T) {
	mw := NewMiddleware(authCfg(keyWith("bad", "sk-bad", "oops")))
	mw.Reload(authCfg(keyWith("bad", "sk-bad", "oops2")))
	mw.AddKey(keyWith("more", "sk-more", "oops3"))
	v := "oops4"
	mw.PatchKey("more", KeyPatch{ExpiresAt: &v})
	mw.SetExpiryAuditHook(nil)
	// A hook set later still discovers the malformed keys that were skipped.
	var rec eventRecorder
	mw.SetExpiryAuditHook(rec.hook)
	if n := len(rec.snapshot()); n != 2 {
		t.Fatalf("late hook saw %d events, want 2", n)
	}
}
