package auth

// usage_test.go - per-key token accumulation and hard daily/monthly quotas.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestAddKeyTokensAccumulates(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys:    []config.KeyConfig{{Name: "k", Key: "sk-k", RateLimit: 1000}},
	})
	mw.AddKeyTokens("k", 100)
	mw.AddKeyTokens("k", 50)
	mw.AddKeyTokens("k", 0)  // ignored
	mw.AddKeyTokens("k", -5) // ignored
	mw.AddKeyTokens("missing", 999)

	_, _, tokensMonth, _, _, _, _, ok := mw.KeyStats("k")
	if !ok {
		t.Fatal("key k should exist")
	}
	if tokensMonth != 150 {
		t.Errorf("tokensMonth = %d, want 150", tokensMonth)
	}
}

func TestDailyQuotaEnforced(t *testing.T) {
	const limit = 3
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys:    []config.KeyConfig{{Name: "d", Key: "sk-d", RateLimit: 100000, DailyLimit: limit}},
	})
	h := mw.Handler(okHandler())

	// Exactly `limit` requests succeed; the next is 429.
	for i := 1; i <= limit; i++ {
		rec := fire(h, "sk-d")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i, rec.Code)
		}
	}
	rec := fire(h, "sk-d")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("request %d (over daily limit): got %d, want 429", limit+1, rec.Code)
	}
}

func TestMonthlyQuotaEnforced(t *testing.T) {
	const limit = 2
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys:    []config.KeyConfig{{Name: "m", Key: "sk-m", RateLimit: 100000, MonthlyLimit: limit}},
	})
	h := mw.Handler(okHandler())

	for i := 1; i <= limit; i++ {
		if rec := fire(h, "sk-m"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i, rec.Code)
		}
	}
	if rec := fire(h, "sk-m"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over monthly limit: got %d, want 429", rec.Code)
	}
}

func TestNoQuotaMeansUnlimited(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: true,
		Keys:    []config.KeyConfig{{Name: "u", Key: "sk-u", RateLimit: 100000}}, // no limits
	})
	h := mw.Handler(okHandler())
	for i := 0; i < 50; i++ {
		if rec := fire(h, "sk-u"); rec.Code != http.StatusOK {
			t.Fatalf("unlimited key blocked at request %d with %d", i+1, rec.Code)
		}
	}
}

func fire(h http.Handler, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/generate", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
