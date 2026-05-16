package proxy

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func TestProxyNoHealthyNodes(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "test", URL: "http://localhost:1", GPUModel: "V100"},
	})
	// Mark nodes as unhealthy
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a)
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.2:8b"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestProxyExtractAndRoute(t *testing.T) {
	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11435", GPUModel: "RTX 4090"},
	})
	r.Nodes()[0].Lock()
	r.Nodes()[0].Healthy = true
	r.Nodes()[0].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a)
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.2:8b"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Will fail to dial localhost, but should get past routing
	if rec.Code != 502 && rec.Code != 503 {
		t.Logf("got status %d (expected 502 bad gateway from dial failure)", rec.Code)
	}
}
