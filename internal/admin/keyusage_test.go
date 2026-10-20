package admin

// keyusage_test.go - per-key token totals and estimated cost via /admin/keys.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

func TestHandleKeysReportsTokensAndCost(t *testing.T) {
	cfg := config.Config{
		Savings: config.SavingsConfig{ReferenceCostPer1K: 0.01},
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "team", Key: "sk-team", RateLimit: 1000}},
		},
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	mw := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, mw, cfg)

	// Two requests by key "team" totalling 500 tokens this month.
	s.LogRequest("req-1", "team", "", "llama3", "node-a", "warm", 200, 100, 300, 0, nil)
	s.LogRequest("req-2", "team", "", "llama3", "node-a", "warm", 200, 100, 200, 0, nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
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

// TestHandleKeysReportsDailyMonthlyLimits guards the list output against
// dropping the per-key request quotas: the limits are enforced by auth but were
// once missing from GET /admin/keys, so operators saw 0 (unlimited) for keys
// that were in fact capped. A PATCH must show up in the next list too.
func TestHandleKeysReportsDailyMonthlyLimits(t *testing.T) {
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "team", Key: "sk-team", DailyLimit: 5, MonthlyLimit: 100}},
		},
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	mw := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, mw, cfg)

	list := func() keyResp {
		req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
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
		return keys[0]
	}

	if k := list(); k.DailyLimit != 5 || k.MonthlyLimit != 100 {
		t.Fatalf("limits = (%d, %d), want (5, 100)", k.DailyLimit, k.MonthlyLimit)
	}

	newDaily, newMonthly := 7, 200
	if !mw.PatchKey("team", auth.KeyPatch{DailyLimit: &newDaily, MonthlyLimit: &newMonthly}) {
		t.Fatal("PatchKey returned false for existing key")
	}
	if k := list(); k.DailyLimit != 7 || k.MonthlyLimit != 200 {
		t.Fatalf("limits after patch = (%d, %d), want (7, 200)", k.DailyLimit, k.MonthlyLimit)
	}
}
