package router

import (
	"testing"

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
