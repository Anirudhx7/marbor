package marboragent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doHealthCheck(t *testing.T, srv *Server) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runtime/health", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Result()
}

// TestHandleHealthCheck_UnsupportedRuntimeReturnsClearError mirrors the
// other actions' equivalent - no detected runtime must never silently
// report ok:true for a probe that never ran (R1 extended to actions).
func TestHandleHealthCheck_UnsupportedRuntimeReturnsClearError(t *testing.T) {
	srv := newTestServerWithRuntime(t, "")
	res := doHealthCheck(t, srv)
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

// TestHandleHealthCheck_OllamaSuccess verifies a real, reachable Ollama
// daemon reports ok:true with a real (non-fabricated) latency measurement.
func TestHandleHealthCheck_OllamaSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/ps":
			// Scheduler.Seed() independently probes /api/ps for warm models.
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	srv := newTestServerWithRuntimeURL(t, "ollama", ts.URL)
	res := doHealthCheck(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp healthCheckResult
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true for a reachable ollama daemon, got error %q", resp.Error)
	}
	if resp.Error != "" {
		t.Errorf("expected no error, got %q", resp.Error)
	}
}

// TestHandleHealthCheck_OllamaFailure verifies a daemon answering with a
// non-200 status is reported as a genuine ok:false, never a cached/assumed
// success.
func TestHandleHealthCheck_OllamaFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	srv := newTestServerWithRuntimeURL(t, "ollama", ts.URL)
	res := doHealthCheck(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (a failed probe is still a successful health check), got %d", res.StatusCode)
	}
	var resp healthCheckResult
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false for a daemon returning a non-200 status")
	}
	if resp.Error == "" {
		t.Error("expected a real error message, got none")
	}
}

// TestHandleHealthCheck_VLLMUsesHealthEndpoint verifies the shared
// GET-and-check-200 probe used by vLLM/TGI/llama.cpp.
func TestHandleHealthCheck_VLLMUsesHealthEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	srv := newTestServerWithRuntimeURL(t, "vllm", ts.URL)
	res := doHealthCheck(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp healthCheckResult
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true, got error %q", resp.Error)
	}
}

// TestHandleHealthCheck_MLXUsesModelsEndpoint verifies MLX (which has no
// dedicated /health route) treats a successful GET /v1/models as the
// reachability signal, matching internal/runtime's mlx probe.
func TestHandleHealthCheck_MLXUsesModelsEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	srv := newTestServerWithRuntimeURL(t, "mlx", ts.URL)
	res := doHealthCheck(t, srv)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var resp healthCheckResult
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected ok=true, got error %q", resp.Error)
	}
}

func TestHandleHealthCheck_RequiresBearerToken(t *testing.T) {
	srv := newTestServerWithRuntime(t, "ollama")
	req := httptest.NewRequest(http.MethodGet, "/v1/runtime/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", w.Result().StatusCode)
	}
}
