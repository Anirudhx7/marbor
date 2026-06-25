package admin

// keyusage_test.go - per-key token totals and estimated cost via /admin/keys.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func TestHandleKeysReportsTokensAndCost(t *testing.T) {
	cfg := config.Config{
		Savings: config.SavingsConfig{ReferenceCostPer1K: 0.01},
		Auth: config.AuthConfig{
			Enabled:    true,
			AdminToken: "admin-tok",
			Keys:       []config.KeyConfig{{Name: "team", Key: "sk-team", RateLimit: 1000}},
		},
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	mw := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, mw, cfg)

	// Two requests by key "team" totalling 500 tokens this month.
	s.LogRequest("team", "", "llama3", "node-a", "warm", 100, 300)
	s.LogRequest("team", "", "llama3", "node-a", "warm", 100, 200)

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var keys []keyResp
	if err := json.NewDecoder(rec.Body).Decode(&keys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("len(keys) = %d, want 1", len(keys))
	}
	k := keys[0]
	if k.TokensThisMonth != 500 {
		t.Errorf("tokensThisMonth = %d, want 500", k.TokensThisMonth)
	}
	// 500 tokens / 1000 * 0.01 = 0.005
	want := 500.0 / 1000.0 * 0.01
	if k.EstimatedCostUsd != want {
		t.Errorf("estimatedCostUsd = %f, want %f", k.EstimatedCostUsd, want)
	}
}
