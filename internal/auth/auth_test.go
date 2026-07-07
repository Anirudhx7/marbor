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
		Enabled: config.BoolPtr(true),
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
		Enabled: config.BoolPtr(true),
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

// TestRateLimitZeroMeansUnlimited guards the regression where a key with
// rate_limit:0 (the value the admin API assigns by default, meaning "no cap")
// was rejected with 429 on every request because the token bucket was built
// with capacity 0. Zero must mean unlimited, matching daily/monthly quota.
func TestRateLimitZeroMeansUnlimited(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "unlimited", Key: "sk-unlim", RateLimit: 0},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-unlim")

	// Many consecutive requests must all be allowed; none may 429.
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 (rate_limit:0 must be unlimited)", i+1, rec.Code)
		}
	}
}

func TestRetryAfterHeaderOnRateLimit(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
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
		Enabled: config.BoolPtr(true),
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
		Enabled: config.BoolPtr(true),
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

func TestPatchKey(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "pkey", Key: "sk-patch", RateLimit: 10, DailyLimit: 100, MonthlyLimit: 1000, Models: []string{"llama3"}},
		},
	})

	// Patch rate limit, daily limit, monthly limit, and models.
	newRate := 50
	newDaily := 200
	newMonthly := 5000
	if !mw.PatchKey("pkey", KeyPatch{
		RateLimit:    &newRate,
		DailyLimit:   &newDaily,
		MonthlyLimit: &newMonthly,
		Models:       []string{"llama3", "mistral"},
	}) {
		t.Fatal("PatchKey returned false for existing key")
	}

	_, _, _, models, _, rl, _, ok := mw.KeyStats("pkey")
	if !ok {
		t.Fatal("key not found after patch")
	}
	if rl != 50 {
		t.Errorf("rate_limit = %d, want 50", rl)
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want [llama3 mistral]", models)
	}
}

func TestPatchKeyUsdCaps(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "ukey", Key: "sk-usdcap", RateLimit: 10},
		},
	})

	if daily, monthly, ok := mw.KeyUsdCaps("ukey"); !ok || daily != 0 || monthly != 0 {
		t.Fatalf("pre-patch KeyUsdCaps = (%v, %v, %v), want (0, 0, true)", daily, monthly, ok)
	}

	newDailyCap := 5.0
	newMonthlyCap := 50.0
	if !mw.PatchKey("ukey", KeyPatch{DailyUsdCap: &newDailyCap, MonthlyUsdCap: &newMonthlyCap}) {
		t.Fatal("PatchKey returned false for existing key")
	}

	daily, monthly, ok := mw.KeyUsdCaps("ukey")
	if !ok {
		t.Fatal("key not found after patch")
	}
	if daily != 5.0 {
		t.Errorf("dailyUsdCap = %v, want 5.0", daily)
	}
	if monthly != 50.0 {
		t.Errorf("monthlyUsdCap = %v, want 50.0", monthly)
	}
}

func TestPatchKeyNotFound(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	rate := 10
	if mw.PatchKey("nonexistent", KeyPatch{RateLimit: &rate}) {
		t.Error("PatchKey should return false for unknown key")
	}
}

func TestPatchKeyPreservesCounters(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "cnt", Key: "sk-cnt2", RateLimit: 100},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Fire one request to bump counters.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk-cnt2")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	today, month, _, _, _, _, _, _ := mw.KeyStats("cnt")
	if today != 1 || month != 1 {
		t.Fatalf("pre-patch counters: today=%d month=%d, want 1/1", today, month)
	}

	// Patch rate limit only.
	newRate := 200
	mw.PatchKey("cnt", KeyPatch{RateLimit: &newRate})

	// Counters must survive.
	today2, month2, _, _, _, _, _, _ := mw.KeyStats("cnt")
	if today2 != 1 || month2 != 1 {
		t.Errorf("post-patch counters: today=%d month=%d, want 1/1 (counters must survive patch)", today2, month2)
	}
}
