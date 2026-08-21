package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

// TestProxy_OllamaPath_NoOllamaNodes: POST /api/chat with only a vLLM node
// must return 503 with a clear message directing the caller to /v1/.
func TestProxy_OllamaPath_NoOllamaNodes(t *testing.T) {
	// Mock backend that always returns 200 - should never be reached.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer backend.Close()

	r := router.New(
		config.RoutingConfig{
			Strategy:       "warm-first",
			Fallback:       "least-connections",
			PollIntervalMs: 2000,
		},
		[]config.NodeConfig{
			{Name: "vllm-node", URL: backend.URL, Runtime: "vllm"},
		},
		nil,
	)

	// Mark node healthy with the model loaded.
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no Ollama nodes available") {
		t.Errorf("body = %q, want message containing 'no Ollama nodes available'", body)
	}
	if !strings.Contains(body, "/v1/") {
		t.Errorf("body = %q, want message referencing /v1/ endpoint", body)
	}
}

// TestProxy_V1Path_VLLMNode: POST /v1/chat/completions with a vLLM node must
// route successfully (200 from mock node, no runtime filter applied).
func TestProxy_V1Path_VLLMNode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"total_tokens":10}}`))
	}))
	defer backend.Close()

	r := router.New(
		config.RoutingConfig{
			Strategy:       "warm-first",
			Fallback:       "least-connections",
			PollIntervalMs: 2000,
		},
		[]config.NodeConfig{
			{Name: "vllm-node", URL: backend.URL, Runtime: "vllm"},
		},
		nil,
	)

	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestProxy_LegacyEmptyRuntime_ApiPath: a node with Runtime="" (pre-Validate,
// legacy config) on an /api/chat request must NOT be routed - it is excluded
// by the runtimeFilter="ollama" check (""!="ollama") and the request falls
// through to 503 (no cloud configured). In production, config.Validate()
// always sets Runtime="ollama" as the default so this edge case only appears
// in tests or configs that bypass Validate().
func TestProxy_LegacyEmptyRuntime_ApiPath(t *testing.T) {
	// Mock backend that should never be reached.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true}`))
	}))
	defer backend.Close()

	r := router.New(
		config.RoutingConfig{
			Strategy:       "warm-first",
			Fallback:       "least-connections",
			PollIntervalMs: 2000,
		},
		[]config.NodeConfig{
			// Runtime deliberately left "" to simulate pre-Validate legacy config.
			{Name: "legacy-node", URL: backend.URL, Runtime: ""},
		},
		nil,
	)

	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Runtime="" node is excluded from runtimeFilter="ollama"; no cloud configured
	// so we expect 503.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (Runtime=\"\" node must not serve /api/ paths)", rec.Code)
	}
}

// TestProxy_OllamaPath_OllamaNode: POST /api/chat with an Ollama node must
// route to that node (200 from mock).
func TestProxy_OllamaPath_OllamaNode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true,"eval_count":5,"prompt_eval_count":3}`))
	}))
	defer backend.Close()

	r := router.New(
		config.RoutingConfig{
			Strategy:       "warm-first",
			Fallback:       "least-connections",
			PollIntervalMs: 2000,
		},
		[]config.NodeConfig{
			{Name: "ollama-node", URL: backend.URL, Runtime: "ollama"},
		},
		nil,
	)

	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}
