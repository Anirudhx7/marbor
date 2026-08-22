package marboragent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
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
	srv := &Server{Token: "tok", Version: "v-test"}
	srv.SetScheduler(s)
	return srv
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

func doDeleteModel(t *testing.T, srv *Server, model string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/models/"+model, nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Result()
}

func doUnloadModel(t *testing.T, srv *Server, model string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/models/"+model, nil)
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

// TestHandleListModels_OllamaCapturesFamily guards against the "does not
// support chat" benchmark failure caused by embedding-only models (e.g.
// mxbai-embed, family "bert") being indistinguishable from chat models in
// the models.list response - a caller (the marbor's Benchmark page) needs
// Family to filter them out. A chat model with no family reported must stay
// empty rather than a fabricated guess (R1).
func TestHandleListModels_OllamaCapturesFamily(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[
				{"name":"hf.co/mixedbread-ai/mxbai-embed-large-v1:F16","size":769089536,"details":{"family":"bert"}},
				{"name":"gemma4:latest","size":2223334444,"details":{"family":"gemma3"}}
			]}`))
		case "/api/ps":
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
	if resp.Models[0].Family != "bert" {
		t.Errorf("mxbai-embed Family = %q, want %q", resp.Models[0].Family, "bert")
	}
	if resp.Models[1].Family != "gemma3" {
		t.Errorf("gemma4 Family = %q, want %q", resp.Models[1].Family, "gemma3")
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

// TestHandleDeleteModel_UnsupportedRuntimeReturnsClearError mirrors the pull
// and list handlers' equivalent - no detected runtime must never silently
// report ok:true for a delete that never happened (R1 extended to actions).
func TestHandleDeleteModel_UnsupportedRuntimeReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "")
	res := doDeleteModel(t, srv, "org/repo")
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

// TestHandleDeleteModel_OllamaMissingBinaryReturnsClearError covers the
// ollama family the same way the pull handler's tool-missing tests cover
// vllm/mlx: this test environment has no `ollama` binary on PATH, so the
// delete must fail loudly (never fake success) - the same honest-failure
// shape as a real node missing the CLI.
func TestHandleDeleteModel_OllamaMissingBinaryReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	res := doDeleteModel(t, srv, "llama3:8b")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when the ollama binary is not on PATH")
	}
}

// TestHandleDeleteModel_HFCacheRemovesRealDirectory verifies the non-Ollama
// branch actually removes the model's directory from the real HF cache -
// the closest equivalent to "delete" those runtimes have.
func TestHandleDeleteModel_HFCacheRemovesRealDirectory(t *testing.T) {
	tmp := t.TempDir()
	hub := filepath.Join(tmp, "hub")
	modelDir := filepath.Join(hub, "models--org--repo", "snapshots", "abc123")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HF_HOME", tmp)

	srv := newTestServerWithRuntime(t, "vllm")
	res := doDeleteModel(t, srv, "org/repo")
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, body)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true, got error %q", resp.Error)
	}
	if _, err := os.Stat(filepath.Join(hub, "models--org--repo")); !os.IsNotExist(err) {
		t.Errorf("expected model directory to be removed, stat err = %v", err)
	}
}

// TestHandleDeleteModel_HFCacheModelNotFoundReturnsClearError verifies a
// model that was never downloaded produces an honest "not found" error, not
// a fabricated success (R1 extended to actions).
func TestHandleDeleteModel_HFCacheModelNotFoundReturnsClearError(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "hub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HF_HOME", tmp)

	srv := newTestServerWithRuntime(t, "llamacpp")
	res := doDeleteModel(t, srv, "org/never-downloaded")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

func TestHandleDeleteModel_RequiresBearerToken(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	req := httptest.NewRequest(http.MethodDelete, "/v1/models/llama3:8b", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", w.Result().StatusCode)
	}
}

// TestDeleteViaHFCache_PathTraversalRejected verifies a model name crafted
// to escape the HF cache directory (via the OS's own path separator, not
// "/" - which the "--" replacement in deleteViaHFCache already neutralizes
// on its own) is rejected by the explicit inside-hfCacheDir() check, and
// that nothing outside the cache is ever touched. model comes straight off
// the request path (attacker-influenced), so this is a real, not
// hypothetical, guard for a destructive filesystem operation.
func TestDeleteViaHFCache_PathTraversalRejected(t *testing.T) {
	tmp := t.TempDir()
	hub := filepath.Join(tmp, "hub")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(tmp, "secret-outside-cache")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HF_HOME", tmp)

	sep := string(filepath.Separator)
	traversal := "foo" + sep + ".." + sep + ".." + sep + "secret-outside-cache"
	if err := deleteViaHFCache(context.Background(), traversal, "", ""); err == nil {
		t.Fatal("expected an error rejecting a path-traversal model name")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("secret directory outside the cache must never be touched: %v", err)
	}
}

// TestDeleteViaHFCache_SymlinkRejected verifies a "models--org--repo" entry
// that is itself a symlink is rejected rather than followed - the prefix
// check only proves the symlink's own path lives inside the cache dir, not
// what it resolves to, so os.RemoveAll must never be handed a path whose
// final component is a symlink pointing outside the cache. Skipped on
// windows: os.Symlink there requires elevated privileges CI doesn't grant.
func TestDeleteViaHFCache_SymlinkRejected(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on windows")
	}
	tmp := t.TempDir()
	hub := filepath.Join(tmp, "hub")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(tmp, "secret-outside-cache")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(hub, "models--org--repo")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("HF_HOME", tmp)

	if err := deleteViaHFCache(context.Background(), "org/repo", "", ""); err == nil {
		t.Fatal("expected an error rejecting a symlinked cache entry")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("secret directory outside the cache must never be touched: %v", err)
	}
}

// TestHandleUnloadModel_UnsupportedRuntimeReturnsClearError mirrors the
// pull/list/delete handlers' equivalent - a runtime with no unload
// primitive in unloadCommands (vLLM/TGI/MLX today, per P31's
// verify-before-build) must never silently report ok:true. llamacpp is
// covered separately below (TestHandleUnloadModel_LlamaCpp*) since it is in
// unloadCommands but must still refuse when router mode isn't confirmed.
func TestHandleUnloadModel_UnsupportedRuntimeReturnsClearError(t *testing.T) {
	for _, rt := range []string{"", "vllm", "tgi", "mlx"} {
		srv := newTestServerWithRuntime(t, rt)
		res := doUnloadModel(t, srv, "org/repo")
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("runtime %q: expected 422, got %d", rt, res.StatusCode)
		}
		var resp actionResponse
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			t.Fatalf("runtime %q: decode: %v", rt, err)
		}
		if resp.OK {
			t.Errorf("runtime %q: expected ok=false", rt)
		}
		if !strings.Contains(resp.Error, "unsupported") && !strings.Contains(resp.Error, "no inference runtime detected") {
			t.Errorf("runtime %q: unexpected error message: %q", rt, resp.Error)
		}
	}
}

// TestHandleUnloadModel_OllamaMissingBinaryReturnsClearError covers the
// ollama case the same way the pull/delete handlers' tool-missing tests do:
// this test environment has no `ollama` binary on PATH, so unload must fail
// loudly (never fake success).
func TestHandleUnloadModel_OllamaMissingBinaryReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	res := doUnloadModel(t, srv, "llama3:8b")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when the ollama binary is not on PATH")
	}
}

// TestHandleUnloadModel_LlamaCppPlainServerReturnsClearError covers the
// llamacpp-but-not-router-mode case: internal/runtime.DetectRuntime's
// "llamacpp" signature (a non-empty GET /v1/models) matches a plain
// single-model llama-server too, and a plain server has no /models router
// endpoint at all (404 here, matching real llama-server behavior) - unload
// must refuse with a clear reason, never silently report ok:true.
func TestHandleUnloadModel_LlamaCppPlainServerReturnsClearError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fake.Close()

	srv := newTestServerWithRuntimeURL(t, "llamacpp", fake.URL)
	res := doUnloadModel(t, srv, "my-model.gguf")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when router mode is not detected")
	}
	if !strings.Contains(resp.Error, "router mode not detected") {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

// TestHandleUnloadModel_LlamaCppRouterModeSucceeds covers the genuine router
// mode case: GET /models answers like the real router (README-confirmed
// shape), so unload should confirm router mode and POST /models/unload.
func TestHandleUnloadModel_LlamaCppRouterModeSucceeds(t *testing.T) {
	var unloadBody map[string]string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"my-model.gguf","status":{"value":"loaded"}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/models/unload":
			_ = json.NewDecoder(r.Body).Decode(&unloadBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	srv := newTestServerWithRuntimeURL(t, "llamacpp", fake.URL)
	res := doUnloadModel(t, srv, "my-model.gguf")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true, got error %q", resp.Error)
	}
	if unloadBody["model"] != "my-model.gguf" {
		t.Errorf("expected /models/unload body model=my-model.gguf, got %v", unloadBody)
	}
}

// TestHandleUnloadModel_LlamaCppRouterReportsFailure covers the router
// answering both probes but reporting success:false (e.g. unknown model
// name) - must surface as an error, never as ok:true.
func TestHandleUnloadModel_LlamaCppRouterReportsFailure(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"other-model.gguf","status":{"value":"loaded"}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/models/unload":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	srv := newTestServerWithRuntimeURL(t, "llamacpp", fake.URL)
	res := doUnloadModel(t, srv, "not-loaded.gguf")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when the router reports success:false")
	}
}

// TestHandleUnloadModel_LlamaCppRouterResolvesRepoIDToRouterID covers P34:
// marbor callers send the "org/repo" Hugging Face identifier (hfRepoID/
// hfCacheRepoID's format, used everywhere else HF-cache-sourced models are
// named), but the router's own "id" is a bare filename stem with no
// substring relationship to "org/repo" - confirmed 2026-07-28 against a real
// router-mode instance. Unload must resolve org/repo to the router id via
// each entry's status.args "--model" path before POSTing, rather than
// sending "org/repo" straight through (which 400s "model is not found").
func TestHandleUnloadModel_LlamaCppRouterResolvesRepoIDToRouterID(t *testing.T) {
	var unloadBody map[string]string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"Qwen2.5-0.5B-Instruct-Q4_K_M","status":{"value":"unloaded","args":["/app/llama-server","--model","/hub/models--bartowski--Qwen2.5-0.5B-Instruct-GGUF/snapshots/abc/Qwen2.5-0.5B-Instruct-Q4_K_M.gguf"]}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/models/unload":
			_ = json.NewDecoder(r.Body).Decode(&unloadBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	srv := newTestServerWithRuntimeURL(t, "llamacpp", fake.URL)
	res := doUnloadModel(t, srv, "bartowski/Qwen2.5-0.5B-Instruct-GGUF")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true, got error %q", resp.Error)
	}
	if unloadBody["model"] != "Qwen2.5-0.5B-Instruct-Q4_K_M" {
		t.Errorf("expected resolved router id sent to /models/unload, got %v", unloadBody)
	}
}

// TestHandleUnloadModel_LlamaCppRouterAmbiguousRepoIsClearError covers a repo
// with multiple quant files (the common case for GGUF repos) - "org/repo"
// alone cannot disambiguate which quant to unload, so this must refuse with
// a clear, candidate-listing error rather than guessing one (R1).
func TestHandleUnloadModel_LlamaCppRouterAmbiguousRepoIsClearError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[
				{"id":"Qwen2.5-0.5B-Instruct-Q4_K_M","status":{"value":"unloaded","args":["/app/llama-server","--model","/hub/models--bartowski--Qwen2.5-0.5B-Instruct-GGUF/snapshots/abc/Qwen2.5-0.5B-Instruct-Q4_K_M.gguf"]}},
				{"id":"Qwen2.5-0.5B-Instruct-Q5_K_M","status":{"value":"unloaded","args":["/app/llama-server","--model","/hub/models--bartowski--Qwen2.5-0.5B-Instruct-GGUF/snapshots/abc/Qwen2.5-0.5B-Instruct-Q5_K_M.gguf"]}}
			]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/models/unload":
			t.Fatal("must not POST /models/unload when the repo name is ambiguous")
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	srv := newTestServerWithRuntimeURL(t, "llamacpp", fake.URL)
	res := doUnloadModel(t, srv, "bartowski/Qwen2.5-0.5B-Instruct-GGUF")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false for an ambiguous repo name")
	}
	if !strings.Contains(resp.Error, "ambiguous") || !strings.Contains(resp.Error, "Qwen2.5-0.5B-Instruct-Q4_K_M") || !strings.Contains(resp.Error, "Qwen2.5-0.5B-Instruct-Q5_K_M") {
		t.Errorf("expected error listing both candidate ids, got %q", resp.Error)
	}
}

// TestHandleUnloadModel_LlamaCppRouterUnknownRepoIsClearError covers a repo
// with no loaded router preset matching it at all - must be a clear
// not-found error, never a false success.
func TestHandleUnloadModel_LlamaCppRouterUnknownRepoIsClearError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"other-model","status":{"value":"unloaded","args":["/app/llama-server","--model","/hub/models--other--repo/snapshots/abc/other-model.gguf"]}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/models/unload":
			t.Fatal("must not POST /models/unload when no preset matches the repo")
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	srv := newTestServerWithRuntimeURL(t, "llamacpp", fake.URL)
	res := doUnloadModel(t, srv, "bartowski/Qwen2.5-0.5B-Instruct-GGUF")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp actionResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false when no router preset matches the repo")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

func TestHandleUnloadModel_RequiresBearerToken(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	req := httptest.NewRequest(http.MethodPost, "/v1/models/llama3:8b", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", w.Result().StatusCode)
	}
}

// TestPOSTModelsRouteShape_PullVsUnloadDisambiguation is a regression guard
// for the route-shape decision recorded in .local/specs/node-agent.md: POST
// /v1/models (no trailing segment) must always mean "pull" and POST
// /v1/models/{name...} must always mean "unload" - both are POST, on
// deliberately different path shapes, and a future change that makes them
// collide would silently misroute pulls as unloads or vice versa.
func TestPOSTModelsRouteShape_PullVsUnloadDisambiguation(t *testing.T) {
	srv := newTestServerWithRuntime(t, "")

	pullRes := doPull(t, srv, `{"model":"org/repo"}`)
	var pullResp actionResponse
	if err := json.NewDecoder(pullRes.Body).Decode(&pullResp); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if !strings.Contains(pullResp.Error, "no pull primitive") && !strings.Contains(pullResp.Error, "no inference runtime detected") {
		t.Errorf("POST /v1/models routed somewhere other than the pull handler: %q", pullResp.Error)
	}

	unloadRes := doUnloadModel(t, srv, "org/repo")
	var unloadResp actionResponse
	if err := json.NewDecoder(unloadRes.Body).Decode(&unloadResp); err != nil {
		t.Fatalf("decode unload response: %v", err)
	}
	if !strings.Contains(unloadResp.Error, "no unload primitive") && !strings.Contains(unloadResp.Error, "no inference runtime detected") {
		t.Errorf("POST /v1/models/{name} routed somewhere other than the unload handler: %q", unloadResp.Error)
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

// TestStripANSI guards against a real production report: ollama CLI's
// stderr spinner output (cursor-hide, synchronized-update mode, cursor
// positioning, erase-line, cursor-show) rode along uncleaned into a
// "model not found" delete error, rendering as garbled box characters in
// the admin UI's confirm dialog.
func TestStripANSI(t *testing.T) {
	in := "\x1b[?25l\x1b[?2026h\x1b[?25l\x1b[1G\x1b[K\x1b[?25h\x1b[?2026lError: model 'org/repo:BF16' not found"
	want := "Error: model 'org/repo:BF16' not found"
	if got := stripANSI(in); got != want {
		t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
	}
	if got := stripANSI("plain error, no escapes"); got != "plain error, no escapes" {
		t.Errorf("stripANSI should be a no-op on plain text, got %q", got)
	}
}

// TestLastMeaningfulLine guards against a real production report: a failed
// pull's stderr held hundreds of repeated "pulling <digest>: 100%" lines (one
// per progress tick, since ollama isn't attached to a real TTY as a
// subprocess) followed by the actual "Error: file does not exist" - runDownload
// used to return the whole transcript as the error, blowing up the admin
// UI's pull toast into an unreadable, viewport-covering wall of text.
func TestLastMeaningfulLine(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("pulling 9e6e2841a75f: 100%\n")
	}
	sb.WriteString("Error: file does not exist\n")
	if got := lastMeaningfulLine(sb.String()); got != "Error: file does not exist" {
		t.Errorf("lastMeaningfulLine = %q, want %q", got, "Error: file does not exist")
	}

	if got := lastMeaningfulLine("single line, no trailing newline"); got != "single line, no trailing newline" {
		t.Errorf("lastMeaningfulLine(single line) = %q, want the line itself", got)
	}

	if got := lastMeaningfulLine("trailing blank lines\n\n\n"); got != "trailing blank lines" {
		t.Errorf("lastMeaningfulLine(trailing blanks) = %q, want %q", got, "trailing blank lines")
	}

	if got := lastMeaningfulLine(""); got != "" {
		t.Errorf("lastMeaningfulLine(empty) = %q, want empty", got)
	}
}
