package nodeagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServerWithRuntime(t *testing.T, runtime string) *Server {
	t.Helper()
	return newTestServerWithRuntimeURL(t, runtime, "")
}

func newTestServerWithRuntimeURL(t *testing.T, runtime, url string) *Server {
	t.Helper()
	rd := fakeRuntimeDetector{name: runtime, url: url, found: runtime != ""}
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, rd)
	s.Seed()
	return &Server{Token: "tok", Version: "v-test", Scheduler: s}
}

func doPull(t *testing.T, srv *Server, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Result()
}

func doListModels(t *testing.T, srv *Server) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Result()
}

// TestHandlePullModel_UnsupportedRuntimeReturnsClearError verifies an
// unrecognized/undetected runtime never silently no-ops - it must return a
// clear, honest error (R1 extended to actions: an action that didn't happen
// must never report ok:true).
func TestHandlePullModel_UnsupportedRuntimeReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "")
	res := doPull(t, srv, `{"model":"hf.co/org/repo:Q4_K_M"}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false for a node with no detected runtime")
	}
	if !strings.Contains(resp.Error, "no inference runtime detected") {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

// TestHandlePullModel_KnownButUnavailableToolReturnsClearError covers a
// detected runtime (vllm) whose fallback tool (huggingface-cli) isn't on
// PATH in this test environment - must fail loudly, never fake success.
func TestHandlePullModel_KnownButUnavailableToolReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "vllm")
	res := doPull(t, srv, `{"model":"org/repo"}`)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when huggingface-cli is not on PATH")
	}
}

// TestHandlePullModel_MlxUsesHFHubFallback verifies mlx, like vllm/llamacpp,
// falls back to the huggingface-cli download path (mlx-lm has no standalone
// pull command either) - must fail loudly when the tool isn't on PATH, never
// fake success.
func TestHandlePullModel_MlxUsesHFHubFallback(t *testing.T) {
	srv := newTestServerWithRuntime(t, "mlx")
	res := doPull(t, srv, `{"model":"mlx-community/repo"}`)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when huggingface-cli is not on PATH")
	}
}

func TestHandlePullModel_MissingModelIsBadRequest(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	res := doPull(t, srv, `{}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.StatusCode)
	}
}

func TestHandlePullModel_RequiresBearerToken(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader(`{"model":"llama3:8b"}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", w.Result().StatusCode)
	}
}

// TestHandleListModels_OllamaReturnsRealTags verifies the ollama branch
// queries the runtime's own GET /api/tags and reports back real
// names/sizes - never synthetic data.
func TestHandleListModels_OllamaReturnsRealTags(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2:latest","size":2223334444},{"name":"qwen2.5:14b","size":8887776666}]}`))
		case "/api/ps":
			// Scheduler.Seed() independently probes /api/ps for warm models -
			// this fake server stands in for the whole local Ollama API, not
			// just the one route this test cares about.
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	srv := newTestServerWithRuntimeURL(t, "ollama", ts.URL)
	res := doListModels(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp listModelsResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Models))
	}
	if resp.Models[0].Name != "llama3.2:latest" || resp.Models[0].SizeBytes != 2223334444 || resp.Models[0].Source != "ollama-tags" {
		t.Errorf("unexpected first model: %+v", resp.Models[0])
	}
}

// TestHandleListModels_UnsupportedRuntimeReturnsClearError mirrors the pull
// handler's equivalent - no detected runtime must never silently report an
// empty-but-successful list.
func TestHandleListModels_UnsupportedRuntimeReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "")
	res := doListModels(t, srv)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", res.StatusCode)
	}
	var resp struct{ Error string }
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error, "no inference runtime detected") {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

// TestHandleListModels_HFCacheScansRealDirectory verifies the non-Ollama
// branch reports real directory names/sizes from the HF cache, not a
// placeholder.
func TestHandleListModels_HFCacheScansRealDirectory(t *testing.T) {
	tmp := t.TempDir()
	hub := filepath.Join(tmp, "hub")
	modelDir := filepath.Join(hub, "models--org--repo", "snapshots", "abc123")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HF_HOME", tmp)

	srv := newTestServerWithRuntime(t, "vllm")
	res := doListModels(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp listModelsResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Models))
	}
	if resp.Models[0].Name != "org/repo" || resp.Models[0].SizeBytes != 1024 || resp.Models[0].Source != "hf-cache" {
		t.Errorf("unexpected model: %+v", resp.Models[0])
	}
}

// TestHandleListModels_HFCacheMissingReturnsEmptyList verifies a node with
// no HF cache yet returns an honest empty list, never an error - a fresh
// node genuinely has zero downloaded models.
func TestHandleListModels_HFCacheMissingReturnsEmptyList(t *testing.T) {
	t.Setenv("HF_HOME", filepath.Join(t.TempDir(), "does-not-exist"))

	srv := newTestServerWithRuntime(t, "llamacpp")
	res := doListModels(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp listModelsResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 0 {
		t.Errorf("expected empty list, got %d models", len(resp.Models))
	}
}

func TestHandleListModels_RequiresBearerToken(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", w.Result().StatusCode)
	}
}

func TestHFRepoID(t *testing.T) {
	cases := map[string]string{
		"hf.co/org/repo:Q4_K_M": "org/repo",
		"hf.co/org/repo":        "org/repo",
		"org/repo:Q4_K_M":       "org/repo",
		"llama3:8b":             "llama3:8b", // no "/" before ":" - not HF-shaped, left alone
		"org/repo":              "org/repo",
	}
	for in, want := range cases {
		if got := hfRepoID(in); got != want {
			t.Errorf("hfRepoID(%q) = %q, want %q", in, got, want)
		}
	}
}
