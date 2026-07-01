package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// TestAllowlistRejectionDoesNotConsumeBudget verifies that a request rejected by
// the per-key model allow-list (403) refunds the rate-limit token it consumed in
// auth. A key with rate_limit=2 hit 5 times with a disallowed model must return
// 403 every time - never 429 - proving the budget was not burned by blocked
// requests.
func TestAllowlistRejectionDoesNotConsumeBudget(t *testing.T) {
	authMw := auth.NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "k", Key: "sk-k", RateLimit: 2, Models: []string{"allowed-model"}},
		},
	})
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, nil, nil)
	h := NewHandler(r, nil, nil)
	h.SetAuth(authMw)
	wrapped := authMw.Handler(h)

	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest("POST", "/api/generate",
			strings.NewReader(`{"model":"disallowed-model","prompt":"hi"}`))
		req.Header.Set("Authorization", "Bearer sk-k")
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("request %d: status = %d, want 403 (rate-limit must be refunded, not exhausted to 429)", i, rec.Code)
		}
	}
}
