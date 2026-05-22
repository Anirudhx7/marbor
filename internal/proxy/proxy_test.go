package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func TestProxyNoHealthyNodes(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "test", URL: "http://localhost:1", GPUModel: "V100"},
	}, nil)
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

func TestTranslateCloudPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/api/chat", "/v1/chat/completions"},
		{"/api/generate", "/v1/completions"},
		{"/api/embeddings", "/v1/embeddings"},
		{"/api/tags", "/api/tags"},
		{"/unknown/path", "/unknown/path"},
	}
	for _, tc := range cases {
		got := translateCloudPath(tc.in)
		if got != tc.want {
			t.Errorf("translateCloudPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRewriteModelField(t *testing.T) {
	body := []byte(`{"model":"old-model","prompt":"hello"}`)
	out := rewriteModelField(body, "gpt-4o")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("rewriteModelField output invalid JSON: %v", err)
	}
	var model string
	if err := json.Unmarshal(m["model"], &model); err != nil {
		t.Fatalf("model field not a string: %v", err)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}

	// bad JSON returns original
	bad := []byte(`not-json`)
	got := rewriteModelField(bad, "gpt-4o")
	if !bytes.Equal(got, bad) {
		t.Error("expected original bytes returned on bad JSON")
	}
}

func TestProxyFallsBackToCloud(t *testing.T) {
	// Fake cloud backend that records the Authorization header it received
	var gotAuthHeader string
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[]}`))
	}))
	defer cloudSrv.Close()

	clouds := []config.CloudProvider{
		{
			Name:            "fake-openai",
			Provider:        "openai",
			BaseURL:         cloudSrv.URL,
			APIKey:          "test-key",
			DefaultModel:    "gpt-4o",
			CostPer1KTokens: 0.002,
			Enabled:         true,
		},
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, clouds)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a)
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{"model":"llama3.2:8b","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == 503 {
		t.Error("expected cloud fallback, got 503 (no healthy nodes)")
	}
	if gotAuthHeader != "Bearer test-key" {
		t.Errorf("cloud received Authorization = %q, want %q", gotAuthHeader, "Bearer test-key")
	}
}

func TestProxyNoFallbackWhenCloudDisabled(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-x", Enabled: false},
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, clouds)
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
		t.Errorf("status = %d, want 503 when cloud disabled and no healthy nodes", rec.Code)
	}
}

func TestProxyExtractAndRoute(t *testing.T) {
	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11435", GPUModel: "RTX 4090"},
	}, nil)
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
