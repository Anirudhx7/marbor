package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

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
		{"unparseable treated as non-expiring", "soon", false},
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
