package auth

import (
	"net/http"
	"net/http/httptest"
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

	today, month, models, _, _, _, ok := mw.KeyStats("counter")
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
