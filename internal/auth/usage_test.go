package auth

// usage_test.go - per-key token accumulation and hard daily/monthly quotas.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestAddKeyTokensAccumulates(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
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
		Enabled: config.BoolPtr(true),
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
		Enabled: config.BoolPtr(true),
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
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "u", Key: "sk-u", RateLimit: 100000}}, // no limits
	})
	h := mw.Handler(okHandler())
	for i := 0; i < 50; i++ {
		if rec := fire(h, "sk-u"); rec.Code != http.StatusOK {
			t.Fatalf("unlimited key blocked at request %d with %d", i+1, rec.Code)
		}
	}
}

// TestQuotaRejectionDoesNotDriftCounter verifies that a request rejected by
// quota enforcement does not permanently consume a slot against the limit.
// Before the fix, incrementAndStats() ran before the limit check, so each
// rejection permanently counted against today/month - operators saw their
// quota evaporating without any real usage.
func TestQuotaRejectionDoesNotDriftCounter(t *testing.T) {
	const limit = 2
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "drift", Key: "sk-drift", RateLimit: 100000, DailyLimit: limit}},
	})
	h := mw.Handler(okHandler())

	// Use up the entire limit.
	for i := 0; i < limit; i++ {
		if rec := fire(h, "sk-drift"); rec.Code != http.StatusOK {
			t.Fatalf("setup request %d: got %d, want 200", i+1, rec.Code)
		}
	}

	// Fire many requests that must be rejected.
	for i := 0; i < 10; i++ {
		if rec := fire(h, "sk-drift"); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("rejection %d: got %d, want 429", i+1, rec.Code)
		}
	}

	// Counter must still equal the limit (not limit+10).
	today, _, _, _, _, _, _, ok := mw.KeyStats("drift")
	if !ok {
		t.Fatal("key drift should exist")
	}
	if today != limit {
		t.Errorf("counter drifted: today = %d, want %d (rejected requests must not increment)", today, limit)
	}
}

func fire(h http.Handler, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/generate", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
