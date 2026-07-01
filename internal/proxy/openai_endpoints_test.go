package proxy

// openai_endpoints_test.go - integration tests for OpenAI-compatible endpoints.
//
// Tests /v1/completions, /v1/embeddings routing, /v1/models (all + single),
// unsupported endpoint 501 responses, and DELETE /v1/models rejection.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

const (
	oeTestModel  = "llama3.2:8b"
	oeTestKey    = "sk-oe-test-key-001"
	oeTestKeyName = "oe-test"
)

// buildOEStack constructs a handler stack backed by a mock Ollama node.
// mockHandler is called for every request the proxy forwards; pass nil to use
// a default 200-OK NDJSON responder.
func buildOEStack(t *testing.T, mockHandler http.HandlerFunc) (handler http.Handler, mockOllama *httptest.Server) {
	t.Helper()

	if mockHandler == nil {
		mockHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"model":"`+oeTestModel+`","response":"ok","done":true}`)
		}
	}

	mockOllama = httptest.NewServer(mockHandler)
	t.Cleanup(mockOllama.Close)

	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{{Name: "gpu-0", URL: mockOllama.URL, GPUModel: "RTX 4090", Runtime: "ollama"}},
		nil,
	)

	nodes := rtr.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].LoadedModels = []router.ModelInfo{{Name: oeTestModel, SizeVRAM: 8192}}
	nodes[0].Unlock()

	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled:    true,
			AdminToken: "admin-oe-token",
			Keys: []config.KeyConfig{
				{Name: oeTestKeyName, Key: oeTestKey, RateLimit: 1000},
			},
		},
	}

	tmpDB := filepath.Join(t.TempDir(), "oe-audit.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	al := audit.New(st, true)
	t.Cleanup(func() { al.Close() })

	adminSrv := admin.NewServer(rtr, nil, cfg)
	authMW := auth.NewMiddleware(cfg.Auth)
	proxyH := NewHandler(rtr, adminSrv, al)
	handler = authMW.Handler(proxyH)

	return handler, mockOllama
}

func authedReq(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var b *bytes.Reader
	if body != nil {
		b = bytes.NewReader(body)
	} else {
		b = bytes.NewReader([]byte{})
	}
	req := httptest.NewRequest(method, path, b)
	req.Header.Set("Authorization", "Bearer "+oeTestKey)
	return req
}

// Task 4: /v1/completions routes correctly to the mock Ollama node.
func TestV1CompletionsRoutes(t *testing.T) {
	var gotPath string
	handler, _ := buildOEStack(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		// Ollama /v1/completions response shape (OpenAI-compat).
		fmt.Fprintf(w, `{"id":"cmpl-1","object":"text_completion","model":"%s","choices":[{"text":"hello","finish_reason":"stop"}]}`, oeTestModel)
	})

	body, _ := json.Marshal(map[string]any{"model": oeTestModel, "prompt": "say hello"})
	req := authedReq(t, http.MethodPost, "/v1/completions", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("/v1/completions: got status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(gotPath, "/v1/completions") {
		t.Errorf("/v1/completions: upstream got path %q, want /v1/completions", gotPath)
	}
}

// Task 4: /v1/embeddings routes correctly to the mock Ollama node.
func TestV1EmbeddingsRoutes(t *testing.T) {
	var gotPath string
	handler, _ := buildOEStack(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}]}`)
	})

	body, _ := json.Marshal(map[string]any{"model": oeTestModel, "input": "hello world"})
	req := authedReq(t, http.MethodPost, "/v1/embeddings", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("/v1/embeddings: got status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(gotPath, "/v1/embeddings") {
		t.Errorf("/v1/embeddings: upstream got path %q, want /v1/embeddings", gotPath)
	}
}

// Task 1: GET /v1/models returns loaded model in data list.
func TestServeModelsIncludesLoaded(t *testing.T) {
	handler, _ := buildOEStack(t, nil)

	req := authedReq(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models: got %d, want 200", rec.Code)
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object field: got %q, want \"list\"", resp.Object)
	}
	found := false
	for _, m := range resp.Data {
		if m.ID == oeTestModel {
			found = true
			if m.Status != "loaded" {
				t.Errorf("model %q status: got %q, want \"loaded\"", oeTestModel, m.Status)
			}
		}
	}
	if !found {
		t.Errorf("/v1/models: model %q not in response %v", oeTestModel, resp.Data)
	}
}

// Task 2: GET /v1/models/{model} returns the model when it is loaded.
func TestServeModelSingleFound(t *testing.T) {
	handler, _ := buildOEStack(t, nil)

	req := authedReq(t, http.MethodGet, "/v1/models/"+oeTestModel, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models/%s: got %d, want 200; body: %s", oeTestModel, rec.Code, rec.Body.String())
	}

	var entry struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal single model: %v", err)
	}
	if entry.ID != oeTestModel {
		t.Errorf("id: got %q, want %q", entry.ID, oeTestModel)
	}
	if entry.Object != "model" {
		t.Errorf("object: got %q, want \"model\"", entry.Object)
	}
}

// Task 2: GET /v1/models/{model} returns 404 for unknown model.
func TestServeModelSingleNotFound(t *testing.T) {
	handler, _ := buildOEStack(t, nil)

	req := authedReq(t, http.MethodGet, "/v1/models/does-not-exist:latest", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("/v1/models/unknown: got %d, want 404", rec.Code)
	}

	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal 404 body: %v", err)
	}
	if errResp.Error.Code != "model_not_found" {
		t.Errorf("error.code: got %q, want \"model_not_found\"", errResp.Error.Code)
	}
}

// Task 3: Unsupported OpenAI paths return 501 with correct error shape.
func TestUnsupportedOpenAIEndpoints(t *testing.T) {
	handler, _ := buildOEStack(t, nil)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/images/generations"},
		{http.MethodPost, "/v1/audio/transcriptions"},
		{http.MethodPost, "/v1/moderations"},
		{http.MethodPost, "/v1/fine-tuning/jobs"},
		{http.MethodPost, "/v1/files"},
		{http.MethodPost, "/v1/assistants"},
		{http.MethodPost, "/v1/threads"},
		{http.MethodPost, "/v1/batches"},
		{http.MethodPost, "/v1/vector-stores"},
		{http.MethodDelete, "/v1/models/somemodel"},
	}

	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			req := authedReq(t, tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotImplemented {
				t.Errorf("%s %s: got %d, want 501", tc.method, tc.path, rec.Code)
			}

			var errResp struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("unmarshal 501 body: %v\nbody: %s", err, rec.Body.String())
			}
			if errResp.Error.Code != "unsupported_endpoint" {
				t.Errorf("error.code: got %q, want \"unsupported_endpoint\"", errResp.Error.Code)
			}
			if errResp.Error.Type != "invalid_request_error" {
				t.Errorf("error.type: got %q, want \"invalid_request_error\"", errResp.Error.Type)
			}
		})
	}
}
