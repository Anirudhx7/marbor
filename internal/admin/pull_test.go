package admin

import (
	"encoding/json"
	"fmt"
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

func newPullRequest(t *testing.T, s *Server, node, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/"+node+"/pull", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", node)
	return req
}

// waitForJob polls s.pullJobs directly (same package - test has access to
// the unexported field) until the job for node|model reaches a terminal
// status or the timeout elapses. Pull handling is asynchronous (the HTTP
// handler returns 202 immediately and the pull runs in a background
// goroutine), so tests need this instead of asserting on the POST response
// body the way the old synchronous handler allowed.
func waitForJob(t *testing.T, s *Server, node, model string) pullJobSnapshot {
	t.Helper()
	key := node + "|" + model
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.pullsMu.Lock()
		job, ok := s.pullJobs[key]
		s.pullsMu.Unlock()
		if ok {
			snap := job.snapshot()
			if snap.Status != "downloading" {
				return snap
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %q did not reach a terminal state within timeout", key)
	return pullJobSnapshot{}
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

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))

	res := w.Result()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", res.StatusCode)
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "downloading" {
		t.Errorf("expected status=downloading, got %v", resp["status"])
	}

	final := waitForJob(t, s, "gpu-0", "llama3:8b")
	if final.Status != "success" {
		t.Fatalf("expected job to finish success, got %q (err=%q)", final.Status, final.Error)
	}

	var forwarded map[string]interface{}
	if err := json.Unmarshal(receivedBody, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "llama3:8b" {
		t.Errorf("expected forwarded model=llama3:8b, got %v", forwarded["model"])
	}
	if forwarded["stream"] != true {
		t.Errorf("expected forwarded stream=true (progress requires streaming), got %v", forwarded["stream"])
	}
}

// TestHandleNodePull_DedupsConcurrentPullsOfSameModel verifies that two
// concurrent pull requests for the same model on the same node do not both
// proceed - the second must be rejected with 409 rather than racing the
// first's multi-GB download. The dedup state is ephemeral and in-memory only.
func TestHandleNodePull_DedupsConcurrentPullsOfSameModel(t *testing.T) {
	release := make(chan struct{})
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))
	if w1.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("first pull: expected 202, got %d", w1.Result().StatusCode)
	}

	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))
	if w2.Result().StatusCode != http.StatusConflict {
		t.Fatalf("second concurrent pull: expected 409, got %d", w2.Result().StatusCode)
	}

	close(release)
	final := waitForJob(t, s, "gpu-0", "llama3:8b")
	if final.Status != "success" {
		t.Fatalf("expected first pull to finish success, got %q", final.Status)
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

	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))
	if w1.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("first pull: expected 202, got %d", w1.Result().StatusCode)
	}
	waitForJob(t, s, "gpu-0", "llama3:8b")

	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))
	if w2.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("second pull after first completed: expected 202, got %d", w2.Result().StatusCode)
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

	req := newPullRequest(t, s, "does-not-exist", `{"model":"llama3:8b"}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Result().StatusCode)
	}
}

// TestHandleNodePull_DownNodeFailsFast guards against forwarding a confusing
// upstream error (e.g. a stray auth 401 from whatever happens to be
// listening on a dead node's URL) when the real problem is simply that the
// node is unreachable - the handler must reject with a clear reason before
// ever attempting the pull.
func TestHandleNodePull_DownNodeFailsFast(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = false
	nodes[0].Unlock()

	req := newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Result().StatusCode)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "down") {
		t.Fatalf("expected error to mention node is down, got: %s", body)
	}
}

func TestHandleNodePull_MissingModel(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{}`))

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Result().StatusCode)
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
// mechanism behind the originally reported bug: if the admin API's outbound
// HTTP client timeout is shorter than a slow-but-otherwise-successful pull,
// the client call itself fails (context deadline exceeded) and the job
// finishes "failed", even though the node was never unhealthy and would
// have completed the pull. This is what real-world Hugging Face pulls of
// multi-gigabyte GGUF files hit against the old hardcoded 5-minute timeout.
func TestHandleNodePull_ShortTimeoutCausesBadGateway(t *testing.T) {
	withNodePullTimeout(t, 50*time.Millisecond)

	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow-but-successful pull: nothing is written until the
		// "download" finishes.
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/some-org/some-repo:Q4_K_M"}`))
	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Result().StatusCode)
	}

	final := waitForJob(t, s, "gpu-0", "hf.co/some-org/some-repo:Q4_K_M")
	if final.Status != "failed" {
		t.Fatalf("expected job to fail with a too-short pull timeout, got %q", final.Status)
	}
}

// TestHandleNodePull_SlowHFPullSucceedsWithGenerousTimeout is the fix-side
// counterpart to TestHandleNodePull_ShortTimeoutCausesBadGateway: with a
// generous pull timeout (as production now defaults to via nodePullTimeout),
// the same kind of slow pull completes successfully instead of being killed
// mid-download.
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

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/some-org/some-repo:Q4_K_M"}`))
	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Result().StatusCode)
	}

	final := waitForJob(t, s, "gpu-0", "hf.co/some-org/some-repo:Q4_K_M")
	if final.Status != "success" {
		t.Fatalf("expected job to succeed with a generous pull timeout, got %q (err=%q)", final.Status, final.Error)
	}
}

// TestHandleNodePull_SurfacesUpstreamErrorBody documents the fix for the
// originally reported "Bad Gateway" bug: Ollama's own error text (e.g. why a
// gated/invalid Hugging Face tag was rejected) must reach the job's Error
// field instead of being collapsed into a bare "upstream returned 401" - an
// operator can't tell a missing HF token apart from a malformed tag apart
// from a real outage from a status code alone.
func TestHandleNodePull_SurfacesUpstreamErrorBody(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"401 Unauthorized: this repo is gated, pass a valid HF token"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/gated-org/gated-repo:Q4_K_M"}`))
	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Result().StatusCode)
	}

	final := waitForJob(t, s, "gpu-0", "hf.co/gated-org/gated-repo:Q4_K_M")
	if final.Status != "failed" {
		t.Fatalf("expected job to fail, got %q", final.Status)
	}
	if !strings.Contains(final.Error, "gated") || !strings.Contains(final.Error, "HF token") {
		t.Errorf("expected job error to surface upstream's message, got %q", final.Error)
	}
}

// TestHandleNodePull_DispatchesToAgentWhenCapable verifies the mesh routes a
// pull to the node's Node Agent (not the direct Ollama /api/pull path) when
// the node has an agent enabled and reporting "actions.pull_model" - and
// that the mesh's configured Hugging Face token is forwarded per-request
// (node-agent spec section 16). The direct-to-Ollama mock is never hit in
// this scenario, proving dispatch actually took the agent branch.
func TestHandleNodePull_DispatchesToAgentWhenCapable(t *testing.T) {
	ollamaHit := false
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHit = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	var gotAuth, gotBody string
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actions/pull_model" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mockAgent.Close()

	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
		HuggingFace: config.HuggingFaceConfig{Token: "hf_secret123"},
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "agent-secret-token")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentCapabilities = []string{"telemetry", "actions.pull_model"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/some-org/some-repo:Q4_K_M"}`))
	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Result().StatusCode)
	}

	final := waitForJob(t, s, "gpu-0", "hf.co/some-org/some-repo:Q4_K_M")
	if final.Status != "success" {
		t.Fatalf("expected job to succeed via agent, got %q (err=%q)", final.Status, final.Error)
	}
	if final.Method != "agent" {
		t.Errorf("expected job.Method=agent, got %q", final.Method)
	}
	if ollamaHit {
		t.Error("direct Ollama /api/pull path was hit - should have dispatched to the agent instead")
	}
	if gotAuth != "Bearer agent-secret-token" {
		t.Errorf("agent request Authorization = %q, want Bearer agent-secret-token", gotAuth)
	}
	if !strings.Contains(gotBody, "hf_secret123") {
		t.Errorf("expected mesh's configured HF token forwarded to agent, got body %q", gotBody)
	}
}

// TestHandleCancelPull_StopsAnInFlightDownload verifies DELETE .../pull
// marks a downloading job "cancelled" immediately, and that the pull
// goroutine's own eventual result (which arrives moments later, as a side
// effect of the same context cancellation aborting its in-flight
// http.Client.Do call - stdlib-guaranteed behavior, not re-tested here)
// cannot clobber that outcome back to "failed". The admin UI needs to tell
// "I stopped this on purpose" apart from "this broke".
func TestHandleCancelPull_StopsAnInFlightDownload(t *testing.T) {
	release := make(chan struct{})
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))
	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Result().StatusCode)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/admin/v1/nodes/gpu-0/pull?model=llama3:8b", nil)
	delReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	delReq.SetPathValue("name", "gpu-0")
	wDel := httptest.NewRecorder()
	s.Handler().ServeHTTP(wDel, delReq)
	if wDel.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from cancel, got %d", wDel.Result().StatusCode)
	}
	var delResp map[string]interface{}
	if err := json.NewDecoder(wDel.Body).Decode(&delResp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if delResp["cancelled"] != true {
		t.Errorf("expected cancelled=true, got %v", delResp["cancelled"])
	}

	// Cancel takes effect synchronously inside handleCancelPull - the job
	// must already read "cancelled" the instant the DELETE responds, not
	// eventually.
	s.pullsMu.Lock()
	job := s.pullJobs["gpu-0|llama3:8b"]
	s.pullsMu.Unlock()
	if job.snapshot().Status != "cancelled" {
		t.Fatalf("expected job.Status=cancelled immediately after DELETE, got %q", job.snapshot().Status)
	}

	// Let the mock's blocked handler finish naturally (as if the download
	// had raced to completion right as it was cancelled) and confirm the
	// goroutine's own finish() call can't overwrite the cancelled outcome.
	close(release)
	time.Sleep(50 * time.Millisecond)
	if job.snapshot().Status != "cancelled" {
		t.Fatalf("expected job.Status to stay cancelled, got %q", job.snapshot().Status)
	}
}

// TestHandleCancelPull_AlreadyFinishedIsANoOp verifies a cancel arriving
// after the pull already finished doesn't clobber its real outcome.
func TestHandleCancelPull_AlreadyFinishedIsANoOp(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))
	waitForJob(t, s, "gpu-0", "llama3:8b")

	delReq := httptest.NewRequest(http.MethodDelete, "/admin/v1/nodes/gpu-0/pull?model=llama3:8b", nil)
	delReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	delReq.SetPathValue("name", "gpu-0")
	wDel := httptest.NewRecorder()
	s.Handler().ServeHTTP(wDel, delReq)

	var delResp map[string]interface{}
	if err := json.NewDecoder(wDel.Body).Decode(&delResp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if delResp["cancelled"] != false {
		t.Errorf("expected cancelled=false for an already-finished job, got %v", delResp["cancelled"])
	}

	s.pullsMu.Lock()
	job := s.pullJobs["gpu-0|llama3:8b"]
	s.pullsMu.Unlock()
	if job.snapshot().Status != "success" {
		t.Errorf("cancel on a finished job must not overwrite its real outcome, got %q", job.snapshot().Status)
	}
}
