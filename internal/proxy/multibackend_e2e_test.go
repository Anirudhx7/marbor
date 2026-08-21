package proxy

// multibackend_e2e_test.go - end-to-end tests for multi-backend routing.
//
// Covers Ollama, vLLM, TGI, and llama.cpp backends routed through the full
// proxy stack. No real network calls - all backends are httptest.Server mocks.
// Nodes are seeded directly (no background polling) so tests are deterministic.

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

// mockOllamaBackend returns an httptest.Server that responds to /api/chat
// and /api/generate with a minimal NDJSON streaming response.
func mockOllamaBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
		default:
			// /api/chat, /api/generate
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"model":"llama3","done":true,"eval_count":5,"prompt_eval_count":3}` + "\n"))
		}
	}))
}

// mockVLLMBackend returns an httptest.Server that responds to vLLM-style endpoints.
func mockVLLMBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"object":"list","data":[{"id":"llama3","object":"model"}]}`))
		default:
			// /v1/chat/completions
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[],"usage":{"total_tokens":10}}`))
		}
	}))
}

// mockTGIBackend returns an httptest.Server that responds to TGI-style endpoints.
func mockTGIBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/info":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"model_id":"llama3"}`))
		default:
			// /v1/chat/completions
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[],"usage":{"total_tokens":8}}`))
		}
	}))
}

// mockLlamaCppBackend returns an httptest.Server that responds to llama.cpp-style endpoints.
// llama.cpp uses the same /health + /v1/models + /v1/chat/completions surface as vLLM.
func mockLlamaCppBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"object":"list","data":[{"id":"llama3","object":"model"}]}`))
		default:
			// /v1/chat/completions
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[],"usage":{"total_tokens":6}}`))
		}
	}))
}

// buildMultiBackendStack creates the proxy handler for a single-node cluster.
// The node is seeded warm directly - no background polling.
func buildMultiBackendStack(t *testing.T, nodeURL string, runtime string) http.Handler {
	t.Helper()
	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{{Name: "node-0", URL: nodeURL, Runtime: runtime}},
		nil,
	)
	for _, n := range rtr.Nodes() {
		seedNode(n, "llama3")
	}
	a := admin.NewServer(rtr, nil, config.Config{})
	return NewHandler(rtr, a, nil)
}

// TestMultiBackend_Ollama_ApiChat: Ollama node with Runtime="ollama" routes
// POST /api/chat and returns 200 with NDJSON body.
func TestMultiBackend_Ollama_ApiChat(t *testing.T) {
	backend := mockOllamaBackend(t)
	defer backend.Close()

	h := buildMultiBackendStack(t, backend.URL, "ollama")

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "done") {
		t.Errorf("body = %q, want NDJSON containing 'done' field", rec.Body.String())
	}
}

// TestMultiBackend_VLLM_V1Chat: vLLM node with Runtime="vllm" routes
// POST /v1/chat/completions and returns 200.
func TestMultiBackend_VLLM_V1Chat(t *testing.T) {
	backend := mockVLLMBackend(t)
	defer backend.Close()

	h := buildMultiBackendStack(t, backend.URL, "vllm")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestMultiBackend_TGI_V1Chat: TGI node with Runtime="tgi" routes
// POST /v1/chat/completions and returns 200.
func TestMultiBackend_TGI_V1Chat(t *testing.T) {
	backend := mockTGIBackend(t)
	defer backend.Close()

	h := buildMultiBackendStack(t, backend.URL, "tgi")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestMultiBackend_LlamaCpp_V1Chat: llama.cpp node with Runtime="llamacpp"
// routes POST /v1/chat/completions and returns 200.
func TestMultiBackend_LlamaCpp_V1Chat(t *testing.T) {
	backend := mockLlamaCppBackend(t)
	defer backend.Close()

	h := buildMultiBackendStack(t, backend.URL, "llamacpp")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestMultiBackend_MixedCluster_ApiChatGoesToOllama: a cluster with one Ollama
// node and one vLLM node - POST /api/chat must route only to the Ollama node.
// The vLLM node URL is a dead address; if /api/chat went there the test would
// get a 502, not 200. The Ollama mock confirms the request landed correctly.
func TestMultiBackend_MixedCluster_ApiChatGoesToOllama(t *testing.T) {
	ollamaReached := false
	ollamaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaReached = true
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3","done":true,"eval_count":5,"prompt_eval_count":3}` + "\n"))
	}))
	defer ollamaMock.Close()

	vllmMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should never be reached for /api/chat
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer vllmMock.Close()

	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{
			{Name: "ollama-node", URL: ollamaMock.URL, Runtime: "ollama"},
			{Name: "vllm-node", URL: vllmMock.URL, Runtime: "vllm"},
		},
		nil,
	)
	for _, n := range rtr.Nodes() {
		seedNode(n, "llama3")
	}
	a := admin.NewServer(rtr, nil, config.Config{})
	h := NewHandler(rtr, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !ollamaReached {
		t.Error("Ollama mock was not reached; /api/chat routed to wrong node")
	}
}

// TestMultiBackend_MixedCluster_V1ChatSucceeds: a cluster with one Ollama node
// and one vLLM node - POST /v1/chat/completions must succeed (not 503).
// Either node is a valid target for /v1/ paths.
func TestMultiBackend_MixedCluster_V1ChatSucceeds(t *testing.T) {
	ollamaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"total_tokens":10}}`))
	}))
	defer ollamaMock.Close()

	vllmMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"total_tokens":10}}`))
	}))
	defer vllmMock.Close()

	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{
			{Name: "ollama-node", URL: ollamaMock.URL, Runtime: "ollama"},
			{Name: "vllm-node", URL: vllmMock.URL, Runtime: "vllm"},
		},
		nil,
	)
	for _, n := range rtr.Nodes() {
		seedNode(n, "llama3")
	}
	a := admin.NewServer(rtr, nil, config.Config{})
	h := NewHandler(rtr, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestMultiBackend_VLLMOnly_ApiChatReturns503: a cluster with only vLLM nodes
// must return 503 with a helpful message when /api/chat is requested.
func TestMultiBackend_VLLMOnly_ApiChatReturns503(t *testing.T) {
	vllmMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should not be reached for /api/ paths.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer vllmMock.Close()

	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{
			{Name: "vllm-node", URL: vllmMock.URL, Runtime: "vllm"},
		},
		nil,
	)
	for _, n := range rtr.Nodes() {
		seedNode(n, "llama3")
	}
	a := admin.NewServer(rtr, nil, config.Config{})
	h := NewHandler(rtr, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (vLLM-only cluster must reject /api/chat)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no Ollama nodes available") {
		t.Errorf("body = %q, want message containing 'no Ollama nodes available'", body)
	}
}

// TestMultiBackend_EmptyRuntime_ApiChatReturns503: a node with Runtime="" is
// excluded from Ollama-specific routing (runtimeFilter="ollama"). /api/chat
// must return 503 with no cloud configured. In production, config.Validate()
// always sets Runtime="ollama" as the default - this tests the raw edge case.
func TestMultiBackend_EmptyRuntime_ApiChatReturns503(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should not be reached - Runtime="" excludes this node from /api/ paths.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true}`))
	}))
	defer backend.Close()

	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{
			// Runtime="" simulates a legacy or pre-Validate config.
			{Name: "legacy-node", URL: backend.URL, Runtime: ""},
		},
		nil,
	)
	for _, n := range rtr.Nodes() {
		seedNode(n, "llama3")
	}
	a := admin.NewServer(rtr, nil, config.Config{})
	h := NewHandler(rtr, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (Runtime='' node must not serve /api/ paths)", rec.Code)
	}
}
