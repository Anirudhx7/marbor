package auth

import (
	"bytes"
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

// keyExpired reports whether a key must be rejected for its expires_at: past
// its expiry, or malformed (fail closed). Mirrors how the middleware combines
// ExpiryStatus.
func keyExpired(expiresAt string, now time.Time) bool {
	expired, malformed := ExpiryStatus(expiresAt, now)
	return expired || malformed
}

func TestKeyExpired(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"empty never expires", "", false},
		{"date in the past", "2020-01-01", true},
		{"date in the future", "2099-01-01", false},
		{"date-only valid through end of that day", "2026-06-15", false},
		{"date-only expired the next day", "2026-06-14", true},
		{"rfc3339 past", "2026-06-15T11:59:00Z", true},
		{"rfc3339 future", "2026-06-15T12:01:00Z", false},
		{"malformed fails closed", "soon", true},
		{"malformed date fails closed", "2026-13-45", true},
		{"datetime-local past", "2026-06-15T11:59", true},
		{"datetime-local future", "2026-06-15T12:01", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := keyExpired(tt.expiresAt, now); got != tt.want {
				t.Errorf("keyExpired(%q) = %v, want %v", tt.expiresAt, got, tt.want)
			}
		})
	}
}

// TestExpiredKeyRejected is a regression test: a key past its expires_at must be
// rejected with 401 and must not consume rate-limit/quota budget. The field was
// loaded and shown in the UI but never enforced.
func TestExpiredKeyRejected(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "expired", Key: "sk-old", RateLimit: 1000, ExpiresAt: "2020-01-01"},
			{Name: "valid", Key: "sk-new", RateLimit: 1000, ExpiresAt: "2099-01-01"},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name string
		key  string
		want int
	}{
		{"expired key rejected", "Bearer sk-old", http.StatusUnauthorized},
		{"unexpired key allowed", "Bearer sk-new", http.StatusOK},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Authorization", tt.key)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// TestPatchKeyExpiresAt is a regression test: expires_at was only settable at
// key creation, with no way to add, change, or clear it afterward.
func TestPatchKeyExpiresAt(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "k1", Key: "sk-1", RateLimit: 1000, ExpiresAt: "2099-01-01"},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	past := "2020-01-01"
	if !mw.PatchKey("k1", KeyPatch{ExpiresAt: &past}) {
		t.Fatal("PatchKey returned false for existing key")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("after patching expires_at to the past: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	cleared := ""
	if !mw.PatchKey("k1", KeyPatch{ExpiresAt: &cleared}) {
		t.Fatal("PatchKey returned false for existing key")
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("after clearing expires_at: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestExpiryStatus(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in                 string
		expired, malformed bool
	}{
		{"", false, false},
		{"2099-01-01", false, false},
		{"2020-01-01", true, false},
		{"2099-01-01T10:00:00Z", false, false},
		{"2020-01-01T10:00:00Z", true, false},
		{"not-a-date", false, true},
		{"2026/06/15", false, true},
	}
	for _, tt := range tests {
		expired, malformed := ExpiryStatus(tt.in, now)
		if expired != tt.expired || malformed != tt.malformed {
			t.Errorf("ExpiryStatus(%q) = (%v, %v), want (%v, %v)", tt.in, expired, malformed, tt.expired, tt.malformed)
		}
	}
}

// A key whose persisted expires_at is malformed must be rejected with 401, must
// not authenticate, and must produce a warning naming the key but never its
// value.
func TestMalformedPersistedExpiryRejectedAndWarned(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "bad-expiry", Key: "sk-secret-value", RateLimit: 1000, ExpiresAt: "next tuesday"},
			{Name: "good", Key: "sk-good", RateLimit: 1000, ExpiresAt: "2099-01-01"},
			{Name: "none", Key: "sk-none", RateLimit: 1000},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	do := func(key string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := do("sk-secret-value"); got != http.StatusUnauthorized {
		t.Fatalf("malformed-expiry key: status = %d, want 401", got)
	}
	if got := do("sk-good"); got != http.StatusOK {
		t.Errorf("valid date-only expiry: status = %d, want 200", got)
	}
	if got := do("sk-none"); got != http.StatusOK {
		t.Errorf("empty expiry: status = %d, want 200", got)
	}

	do("sk-secret-value") // second rejection within the interval must not warn again
	out := buf.String()
	if !strings.Contains(out, `"bad-expiry"`) {
		t.Errorf("log %q does not name the key", out)
	}
	if strings.Contains(out, "sk-secret-value") {
		t.Errorf("log leaked the key value: %q", out)
	}
	if n := strings.Count(out, "malformed"); n != 1 {
		t.Errorf("malformed-expiry warning logged %d times, want 1 (rate-limited)", n)
	}
}

// syncBuffer is a goroutine-safe log sink for tests that capture log output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
