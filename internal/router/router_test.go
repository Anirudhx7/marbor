package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestRouteWarmFirst(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "RTX 4090"},
		{Name: "gpu-1", URL: "http://localhost:2", GPUModel: "RTX 4090"},
	})
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "llama3.2:8b"}}
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].mu.Unlock()

	node, warm := r.Route("llama3.2:8b")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if node.Name != "gpu-0" {
		t.Errorf("route = %s, want gpu-0", node.Name)
	}
	if !warm {
		t.Error("expected warm routing")
	}

	node, warm = r.Route("unknown")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if warm {
		t.Error("expected cold routing")
	}
}

func TestExtractModelName(t *testing.T) {
	body := []byte(`{"model":"llama3.2:8b","prompt":"hi"}`)
	got := ExtractModelName(body)
	if got != "llama3.2:8b" {
		t.Errorf("model = %q, want llama3.2:8b", got)
	}
}

func TestAllUnhealthy(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	})
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false
	r.nodes[0].mu.Unlock()
	node, _ := r.Route("any")
	if node != nil {
		t.Error("expected nil for all unhealthy")
	}
}

func TestWarmFirstPicksLeastConns(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "RTX 4090"},
		{Name: "gpu-1", URL: "http://localhost:2", GPUModel: "RTX 4090"},
	})
	model := []ModelInfo{{Name: "llama3.2:8b"}}
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = model
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].LoadedModels = model
	r.nodes[1].Healthy = true
	r.nodes[1].mu.Unlock()

	// gpu-0 has more active connections — gpu-1 should win
	r.nodes[0].ActiveConns = 5
	r.nodes[1].ActiveConns = 1

	node, warm := r.Route("llama3.2:8b")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if node.Name != "gpu-1" {
		t.Errorf("route = %s, want gpu-1 (least conns)", node.Name)
	}
	if !warm {
		t.Error("expected warm routing")
	}
}

func TestPollNodeWithMockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3.2:8b", "sizeVram": 4294967296},
			},
		})
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL, GPUModel: "RTX 4090"},
	})

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()

	if !r.nodes[0].Healthy {
		t.Error("expected node healthy after successful poll")
	}
	if len(r.nodes[0].LoadedModels) != 1 {
		t.Fatalf("expected 1 loaded model, got %d", len(r.nodes[0].LoadedModels))
	}
	if r.nodes[0].LoadedModels[0].Name != "llama3.2:8b" {
		t.Errorf("model = %s, want llama3.2:8b", r.nodes[0].LoadedModels[0].Name)
	}
	if r.nodes[0].LoadedModels[0].SizeVRAM != 4294967296 {
		t.Errorf("SizeVRAM = %d, want 4294967296", r.nodes[0].LoadedModels[0].SizeVRAM)
	}
	if r.nodes[0].LastPollAt.IsZero() {
		t.Error("expected LastPollAt to be set")
	}
}

func TestPollNodeMarksUnhealthyOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	})

	// 3 failures needed to mark unhealthy
	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	healthy := r.nodes[0].Healthy
	failures := r.nodes[0].Failures
	r.nodes[0].mu.RUnlock()

	if healthy {
		t.Error("expected node unhealthy after 3 failed polls")
	}
	if failures < 3 {
		t.Errorf("failures = %d, want >= 3", failures)
	}
}

func TestPollNodeUptime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	})
	r.nodes[0].FirstSeenAt = time.Now().Add(-2 * time.Hour)

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	uptime := r.nodes[0].Uptime
	r.nodes[0].mu.RUnlock()

	if uptime == "" {
		t.Error("expected Uptime to be set")
	}
}
