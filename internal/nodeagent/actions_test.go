package nodeagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServerWithRuntime(t *testing.T, runtime string) *Server {
	t.Helper()
	rd := fakeRuntimeDetector{name: runtime, found: runtime != ""}
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, rd)
	s.Seed()
	return &Server{Token: "tok", Version: "v-test", Scheduler: s}
}

func doPull(t *testing.T, srv *Server, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/actions/pull_model", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/actions/pull_model", strings.NewReader(`{"model":"llama3:8b"}`))
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
