package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func newPullTestServer(t *testing.T, nodes []config.NodeConfig) *Server {
	t.Helper()
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys: []config.KeyConfig{
				{Name: "test", Key: "test-token"},
			},
		},
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, nodes, nil)
	return NewServer(r, nil, cfg)
}

func TestHandleNodePull_Success(t *testing.T) {
	var receivedBody []byte
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"llama3:8b"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
	if resp["node"] != "gpu-0" {
		t.Errorf("expected node=gpu-0, got %v", resp["node"])
	}
	if resp["model"] != "llama3:8b" {
		t.Errorf("expected model=llama3:8b, got %v", resp["model"])
	}

	var forwarded map[string]interface{}
	if err := json.Unmarshal(receivedBody, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "llama3:8b" {
		t.Errorf("expected forwarded model=llama3:8b, got %v", forwarded["model"])
	}
	if forwarded["stream"] != false {
		t.Errorf("expected forwarded stream=false, got %v", forwarded["stream"])
	}
}

// TestHandleNodePull_DedupsConcurrentPullsOfSameModel verifies that two
// concurrent pull requests for the same model on the same node do not both
// proceed - the second must be rejected with 409 rather than racing the
// first's multi-GB download. The dedup state is ephemeral and in-memory only.
func TestHandleNodePull_DedupsConcurrentPullsOfSameModel(t *testing.T) {
	reachedUpstream := make(chan struct{})
	release := make(chan struct{})
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reachedUpstream) // signal the first request has reached the upstream call
		<-release
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"llama3:8b"}`
	newPullReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("name", "gpu-0")
		return req
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, newPullReq())
		firstDone <- w
	}()

	<-reachedUpstream // first request is now blocked inside the upstream call

	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, newPullReq())
	if w2.Result().StatusCode != http.StatusConflict {
		t.Fatalf("second concurrent pull: expected 409, got %d", w2.Result().StatusCode)
	}

	close(release)
	w1 := <-firstDone
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("first pull: expected 200, got %d", w1.Result().StatusCode)
	}
}

// TestHandleNodePull_SlotFreedAfterCompletion verifies that once a pull
// finishes, its dedup slot is released so a subsequent pull of the same
// model on the same node is allowed to proceed.
func TestHandleNodePull_SlotFreedAfterCompletion(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"llama3:8b"}`
	newPullReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("name", "gpu-0")
		return req
	}

	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, newPullReq())
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("first pull: expected 200, got %d", w1.Result().StatusCode)
	}

	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, newPullReq())
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("second pull after first completed: expected 200, got %d", w2.Result().StatusCode)
	}
}

func TestHandleSetNodePrewarm_TogglesFlag(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/prewarm", strings.NewReader(`{"disabled":true}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["prewarm_disabled"] != true {
		t.Errorf("expected prewarm_disabled=true, got %v", resp["prewarm_disabled"])
	}
}

func TestHandleSetNodePrewarm_NodeNotFound(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/does-not-exist/prewarm", strings.NewReader(`{"disabled":true}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "does-not-exist")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Result().StatusCode)
	}
}

func TestHandleNodePull_NodeNotFound(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	body := `{"model":"llama3:8b"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/does-not-exist/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "does-not-exist")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", res.StatusCode)
	}
}

func TestHandleNodePull_MissingModel(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.StatusCode)
	}
}

// withNodePullTimeout temporarily overrides the package-level nodePullTimeout
// for the duration of a test, restoring the original value on cleanup. Used
// to exercise both sides of the timeout without making tests take real hours
// (production default) or being flaky (too tight a margin).
func withNodePullTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := nodePullTimeout
	nodePullTimeout = d
	t.Cleanup(func() { nodePullTimeout = orig })
}

// TestHandleNodePull_ShortTimeoutCausesBadGateway documents the exact
// mechanism behind the reported bug: /api/pull is called with "stream":false,
// so Ollama sends nothing back - not even response headers - until the whole
// download finishes. If the admin API's outbound HTTP client timeout is
// shorter than a slow-but-otherwise-successful pull, the client call itself
// fails (context deadline exceeded) and handleNodePull maps that to a 502
// "Bad Gateway", even though the node was never unhealthy and would have
// completed the pull. This is what real-world Hugging Face pulls of
// multi-gigabyte GGUF files hit against the old hardcoded 5-minute timeout.
func TestHandleNodePull_ShortTimeoutCausesBadGateway(t *testing.T) {
	withNodePullTimeout(t, 50*time.Millisecond)

	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow-but-successful pull: nothing is written until the
		// "download" finishes, matching Ollama's stream:false /api/pull.
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"hf.co/some-org/some-repo:Q4_K_M"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 with a too-short pull timeout, got %d", res.StatusCode)
	}
}

// TestHandleNodePull_SlowHFPullSucceedsWithGenerousTimeout is the fix-side
// counterpart to TestHandleNodePull_ShortTimeoutCausesBadGateway: with a
// generous pull timeout (as production now defaults to via nodePullTimeout),
// the same kind of slow, streaming-response-less pull completes successfully
// instead of being killed mid-download and surfaced as 502.
func TestHandleNodePull_SlowHFPullSucceedsWithGenerousTimeout(t *testing.T) {
	withNodePullTimeout(t, 2*time.Second)

	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"hf.co/some-org/some-repo:Q4_K_M"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with a generous pull timeout, got %d", res.StatusCode)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
}

// TestHandleNodePull_SurfacesUpstreamErrorBody documents the fix for the
// reported "Bad Gateway" bug: Ollama's own error text (e.g. why a gated/
// invalid Hugging Face tag was rejected) must reach the admin UI instead of
// being collapsed into a bare "upstream returned 401" - an operator can't
// tell a missing HF token apart from a malformed tag apart from a real
// outage from a status code alone.
func TestHandleNodePull_SurfacesUpstreamErrorBody(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"401 Unauthorized: this repo is gated, pass a valid HF token"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"hf.co/gated-org/gated-repo:Q4_K_M"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}

	var resp map[string]string
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "gated") || !strings.Contains(resp["error"], "HF token") {
		t.Errorf("expected error to surface upstream's message, got %q", resp["error"])
	}
}
