package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestAuthMiddleware(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys: []config.KeyConfig{
			{Name: "test", Key: "sk-test", RateLimit: 1000},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name string
		auth string
		want int
	}{
		{"no auth", "", 401},
		{"bad format", "Basic xyz", 401},
		{"invalid key", "Bearer sk-bad", 401},
		{"valid key", "Bearer sk-test", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestRateLimit(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys: []config.KeyConfig{
			{Name: "limited", Key: "sk-lim", RateLimit: 1},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-lim")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != 429 {
		t.Errorf("second request = %d, want 429", rec2.Code)
	}
}

func TestRetryAfterHeaderOnRateLimit(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys: []config.KeyConfig{
			{Name: "retry-test", Key: "sk-retry", RateLimit: 1},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-retry")

	// First request: consume the token.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}

	// Second request: rate limited - must have Retry-After.
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", rec2.Code)
	}
	ra := rec2.Header().Get("Retry-After")
	if ra == "" {
		t.Error("429 response missing Retry-After header")
	}
	v, err := strconv.Atoi(ra)
	if err != nil || v < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", ra)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys: []config.KeyConfig{
			{Name: "header-test", Key: "sk-hdr", RateLimit: 100},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-hdr")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("missing X-RateLimit-Limit header")
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("missing X-RateLimit-Remaining header")
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("missing X-RateLimit-Reset header")
	}
	if rec.Header().Get("X-RateLimit-Limit") != "100" {
		t.Errorf("X-RateLimit-Limit = %s, want 100", rec.Header().Get("X-RateLimit-Limit"))
	}
}

func TestKeyStats(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys: []config.KeyConfig{
			{Name: "counter", Key: "sk-cnt", RateLimit: 100, Models: []string{"llama3.2:8b"}},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-cnt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	today, month, _, models, _, _, _, ok := mw.KeyStats("counter")
	if !ok {
		t.Fatal("expected key to exist")
	}
	if today != 1 {
		t.Errorf("today = %d, want 1", today)
	}
	if month != 1 {
		t.Errorf("month = %d, want 1", month)
	}
	if len(models) != 1 || models[0] != "llama3.2:8b" {
		t.Errorf("models = %v, want [llama3.2:8b]", models)
	}
}
