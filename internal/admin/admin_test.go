package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func newTestServer() *Server {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{})
}

func TestTrackLocalRequestModel(t *testing.T) {
	s := newTestServer()

	s.TrackLocalRequestModel("llama3", 100)
	s.TrackLocalRequestModel("llama3", 200)
	s.TrackLocalRequestModel("llama3", 0) // token count unavailable

	if got := atomic.LoadInt64(&s.localCount); got != 3 {
		t.Errorf("localCount = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&s.localTokens); got != 300 {
		t.Errorf("localTokens = %d, want 300", got)
	}
}

func TestTrackCloudCostModel(t *testing.T) {
	s := newTestServer()

	s.TrackCloudCostModel("gpt-4o", 0.002, 1000)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)

	if gotCount := atomic.LoadInt64(&s.cloudCount); gotCount != 2 {
		t.Errorf("cloudCount = %d, want 2", gotCount)
	}

	// 0.002 * 1000/1000 + 0.002 * 500/1000 = 0.003 USD
	s.mu.RLock()
	gotSpent := s.cloudSpentUSD
	s.mu.RUnlock()

	wantSpent := 0.002*1000.0/1000.0 + 0.002*500.0/1000.0
	if gotSpent != wantSpent {
		t.Errorf("cloudSpentUSD = %f, want %f", gotSpent, wantSpent)
	}
}

func TestTrackCloudCostModelUnknownTokensAddsNoCost(t *testing.T) {
	s := newTestServer()

	s.TrackCloudCostModel("gpt-4o", 0.002, 0)

	s.mu.RLock()
	gotSpent := s.cloudSpentUSD
	s.mu.RUnlock()
	if gotSpent != 0 {
		t.Errorf("cloudSpentUSD = %f, want 0 (no fabricated cost)", gotSpent)
	}
}

func TestHandleSavings(t *testing.T) {
	s := newTestServer()

	// 3 local requests totaling 1500 tokens, 2 cloud at $0.002/1K tokens
	s.TrackLocalRequestModel("llama3", 500)
	s.TrackLocalRequestModel("llama3", 500)
	s.TrackLocalRequestModel("llama3", 500)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)

	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()

	s.handleSavings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	for _, field := range []string{"local_requests", "cloud_requests", "cloud_spent_usd", "saved_usd"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("response missing field %q", field)
		}
	}

	if got := resp["local_requests"].(float64); got != 3 {
		t.Errorf("local_requests = %v, want 3", got)
	}
	if got := resp["cloud_requests"].(float64); got != 2 {
		t.Errorf("cloud_requests = %v, want 2", got)
	}

	// cloud_spent_usd: 2 * (0.002 * 500/1000) = 0.002
	wantSpent := 0.002 * 500.0 / 1000.0 * 2
	if got := resp["cloud_spent_usd"].(float64); got != wantSpent {
		t.Errorf("cloud_spent_usd = %v, want %v", got, wantSpent)
	}

	// saved_usd: 1500 tokens * $0.002/1K = 0.003
	wantSaved := 1500.0 / 1000.0 * 0.002
	if got := resp["saved_usd"].(float64); got != wantSaved {
		t.Errorf("saved_usd = %v, want %v", got, wantSaved)
	}
}

func TestHandleSavingsCustomReferenceRate(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{Savings: config.SavingsConfig{ReferenceCostPer1K: 0.01}}
	s := NewServer(r, nil, cfg)

	s.TrackLocalRequestModel("llama3", 1500)

	rec := httptest.NewRecorder()
	s.handleSavings(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil))

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// saved_usd: 1500 tokens / 1000 * $0.01 = 0.015
	wantSaved := 1500.0 / 1000.0 * 0.01
	if got := resp["saved_usd"].(float64); got != wantSaved {
		t.Errorf("saved_usd = %v, want %v (custom reference rate must flow from config)", got, wantSaved)
	}

	// The hourly analytics buckets must use the same configured rate.
	var bucketSaved float64
	for _, b := range s.analytics.last24hBuckets() {
		bucketSaved += b.SavedUSD
	}
	if bucketSaved != wantSaved {
		t.Errorf("analytics SavedUSD = %v, want %v (analyticsStore must use configured rate)", bucketSaved, wantSaved)
	}
}

func TestHandleSavingsNullWhenNoTokenData(t *testing.T) {
	s := newTestServer()

	// Requests happened but no token counts could be parsed.
	s.TrackLocalRequestModel("llama3", 0)
	s.TrackCloudCostModel("gpt-4o", 0.002, 0)

	rec := httptest.NewRecorder()
	s.handleSavings(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil))

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if v, ok := resp["saved_usd"]; !ok || v != nil {
		t.Errorf("saved_usd = %v, want null when no token data", v)
	}
	if v, ok := resp["cloud_spent_usd"]; !ok || v != nil {
		t.Errorf("cloud_spent_usd = %v, want null when no token data", v)
	}
}
