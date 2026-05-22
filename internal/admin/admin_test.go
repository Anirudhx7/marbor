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

func TestTrackLocalRequest(t *testing.T) {
	s := newTestServer()

	s.TrackLocalRequest()
	s.TrackLocalRequest()
	s.TrackLocalRequest()

	got := atomic.LoadInt64(&s.localCount)
	if got != 3 {
		t.Errorf("localCount = %d, want 3", got)
	}
}

func TestTrackCloudCost(t *testing.T) {
	s := newTestServer()

	s.TrackCloudCost(0.002)
	s.TrackCloudCost(0.002)

	gotCount := atomic.LoadInt64(&s.cloudCount)
	if gotCount != 2 {
		t.Errorf("cloudCount = %d, want 2", gotCount)
	}

	// Each call: 0.002 * 500 / 1000 = 0.001 USD. Two calls = 0.002 USD.
	s.mu.RLock()
	gotSpent := s.cloudSpentUSD
	s.mu.RUnlock()

	wantSpent := 0.002 * 500.0 / 1000.0 * 2
	if gotSpent != wantSpent {
		t.Errorf("cloudSpentUSD = %f, want %f", gotSpent, wantSpent)
	}
}

func TestHandleSavings(t *testing.T) {
	s := newTestServer()

	// 3 local, 2 cloud at $0.002/1K tokens
	s.TrackLocalRequest()
	s.TrackLocalRequest()
	s.TrackLocalRequest()
	s.TrackCloudCost(0.002)
	s.TrackCloudCost(0.002)

	// Build a request with a valid admin token (default "admin" when auth disabled)
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()

	// Call handleSavings directly (bypassing mux/auth middleware)
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

	// Validate required fields exist
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

	// cloud_spent_usd: 2 * (0.002 * 500 / 1000) = 0.002
	wantSpent := 0.002 * 500.0 / 1000.0 * 2
	if got := resp["cloud_spent_usd"].(float64); got != wantSpent {
		t.Errorf("cloud_spent_usd = %v, want %v", got, wantSpent)
	}

	// saved_usd: 3 * 0.002 * 500 / 1000 = 0.003
	wantSaved := 3.0 * 0.002 * 500.0 / 1000.0
	if got := resp["saved_usd"].(float64); got != wantSaved {
		t.Errorf("saved_usd = %v, want %v", got, wantSaved)
	}
}
