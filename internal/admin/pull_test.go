package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
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
			if !pullJobActive(snap.Status) {
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

// TestHandleNodePull_DiskHardBlock is a regression test: a pull of a curated
// catalog model whose known download size exceeds the node's agent-reported
// free disk must be hard-blocked with 507, before ever reaching the mock
// Ollama server (no confirm-anyway override, unlike VRAM's soft-confirm behavior).
func TestHandleNodePull_DiskHardBlock(t *testing.T) {
	reached := false
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].AgentPresent = true
	nodes[0].DiskFreeGB = 10  // 10GB free
	nodes[0].DiskTotalGB = 20 // real telemetry present
	nodes[0].Unlock()

	// llama3.1:70b's Q4_K_M variant needs ~40000MB (~40GB) - well over 10GB free.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3.1:70b"}`))

	if w.Result().StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("expected 507, got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	if reached {
		t.Error("mock Ollama server was reached - pull should have been blocked before dispatch")
	}
}

// TestHandleNodePull_DiskHardBlock_GenuinelyFullDisk is a regression for a
// code-review catch: a node whose agent legitimately reports 0 GB free (the
// disk really is completely full - a real syscall.Statfs reading, not a
// missing-telemetry placeholder) must still hard-block, not be treated as
// "unknown disk state" and let the pull through. DiskTotalGB > 0 alongside
// DiskFreeGB == 0 is what proves this is real telemetry.
func TestHandleNodePull_DiskHardBlock_GenuinelyFullDisk(t *testing.T) {
	reached := false
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].AgentPresent = true
	nodes[0].DiskFreeGB = 0    // genuinely full - a real reading, not "unreported"
	nodes[0].DiskTotalGB = 500 // proves real telemetry (would be 0 if the agent had never reported disk stats)
	nodes[0].Unlock()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3.2:1b"}`))

	if w.Result().StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 for a genuinely full disk, got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	if reached {
		t.Error("mock Ollama server was reached - a genuinely full disk must block before dispatch")
	}
}

// TestHandleNodePull_RejectsOllamaLibraryTagOnIncompatibleRuntime is a
// regression test for the runtime-compatibility gate: a bare Ollama-library-format tag (e.g. every
// compiled catalog tag) must be rejected up front on a node whose declared
// runtime cannot possibly pull it, before any download starts - not left to
// fail deep inside a cryptic huggingface-cli subprocess error.
func TestHandleNodePull_RejectsOllamaLibraryTagOnIncompatibleRuntime(t *testing.T) {
	reached := false
	mockRuntime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockRuntime.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "vllm-0", URL: mockRuntime.URL, Runtime: "vllm"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "vllm-0", `{"model":"llama3.2:3b"}`))

	if w.Result().StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	if reached {
		t.Error("mock runtime server was reached - an incompatible tag/runtime pull must be rejected before dispatch")
	}
}

// TestHandleNodePull_RejectsGGUFTagOnSafetensorsRuntime covers the second
// compatibility case: an "hf.co/..." GGUF reference (Ollama/llama.cpp's
// own HF-pull convention) must be rejected on vLLM/TGI/MLX, which never load
// GGUF.
func TestHandleNodePull_RejectsGGUFTagOnSafetensorsRuntime(t *testing.T) {
	reached := false
	mockRuntime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockRuntime.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "mlx-0", URL: mockRuntime.URL, Runtime: "mlx"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "mlx-0", `{"model":"hf.co/unsloth/Llama-3.2-3B-GGUF:Q4_K_M"}`))

	if w.Result().StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	if reached {
		t.Error("mock runtime server was reached - a GGUF tag pull to an mlx node must be rejected before dispatch")
	}
}

// TestHandleNodePull_AllowsCompatibleTagsAcrossRuntimes is the flip side of
// the two rejection tests above: a tag/runtime pairing marbor cannot
// confidently call incompatible must still be allowed through to dispatch -
// including a bare Ollama-library tag on an actual Ollama node, a GGUF tag on
// llama.cpp, and an ambiguous bare "org/repo" HF id on every non-GGUF
// runtime (never blocked - see classifyPullTagFormat's doc comment on why).
func TestHandleNodePull_AllowsCompatibleTagsAcrossRuntimes(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		model   string
	}{
		{"ollama-library tag on ollama", "ollama", "llama3.2:3b"},
		{"ollama-library tag on undeclared runtime", "", "llama3.2:3b"},
		{"gguf-hf tag on llamacpp", "llamacpp", "hf.co/unsloth/Llama-3.2-3B-GGUF:Q4_K_M"},
		{"hf-repo tag on vllm", "vllm", "meta-llama/Llama-3.1-8B-Instruct"},
		{"hf-repo tag on mlx", "mlx", "mlx-community/Llama-3.1-8B-Instruct-4bit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached := false
			mockRuntime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status":"success"}`))
			}))
			defer mockRuntime.Close()

			s := newPullTestServer(t, []config.NodeConfig{
				{Name: "node-0", URL: mockRuntime.URL, Runtime: c.runtime},
			})

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, newPullRequest(t, s, "node-0", fmt.Sprintf(`{"model":%q}`, c.model)))

			if w.Result().StatusCode != http.StatusAccepted {
				t.Fatalf("expected 202 (compatible pull allowed through), got %d; body: %s", w.Result().StatusCode, w.Body.String())
			}
			waitForJob(t, s, "node-0", c.model)
			if !reached {
				t.Error("mock runtime server was never reached - a compatible pull must dispatch")
			}
		})
	}
}

// TestHandleNodePull_DiskCheckSkippedWhenUnknown verifies the check never
// blocks (or fabricates a pass) when disk telemetry is unavailable - no
// agent, or an agent that hasn't reported disk stats (e.g. non-Linux host).
func TestHandleNodePull_DiskCheckSkippedWhenUnknown(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})
	// AgentPresent left false (default) - disk state is unknown.

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3.1:70b"}`))

	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 (check skipped, unknown disk state), got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	waitForJob(t, s, "gpu-0", "llama3.1:70b")
}

// TestHandleNodePull_UnresolvableModelHardBlockedOnThinHeadroom is a
// regression test for the unknown-size disk-fit floor: a model tag not in the static catalog (e.g. an HF
// tag, or an uncurated Ollama registry name) has no known download size, so
// classifyDiskFit's size-vs-free-space test cannot run - but that must no
// longer mean the pull sails through unchecked. classifyUnknownSizeDiskFit's
// conservative floor (10% of total, 5GB absolute minimum) still applies to
// the node's current headroom. Before this fix, this exact case (1GB free
// of 20GB - 5%, under both floors) returned 202 and dispatched the pull;
// it must now be hard-blocked before dispatch, exactly like a known-size
// disk overrun.
func TestHandleNodePull_UnresolvableModelHardBlockedOnThinHeadroom(t *testing.T) {
	reached := false
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].AgentPresent = true
	nodes[0].DiskFreeGB = 1 // 1GB of 20GB (5%) - below both the fraction and absolute floor
	nodes[0].DiskTotalGB = 20
	nodes[0].Unlock()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/someorg/somerepo:Q4_K_M"}`))

	if w.Result().StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 (unresolvable model, thin headroom must hard-block), got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	if reached {
		t.Error("mock Ollama server was reached - an unresolvable-size pull on thin headroom must be blocked before dispatch")
	}
}

// TestHandleNodePull_UnresolvableModelAllowedWithHealthyHeadroom is the flip
// side of the hard-block test above: an unresolvable-size tag must still be
// allowed through when the node has comfortable free disk, proving this
// fix is a genuine floor (blocks only when headroom is thin) and not a
// blanket block on every non-catalog pull.
func TestHandleNodePull_UnresolvableModelAllowedWithHealthyHeadroom(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].AgentPresent = true
	nodes[0].DiskFreeGB = 500 // 500GB of 1000GB (50%) - comfortably above both floors
	nodes[0].DiskTotalGB = 1000
	nodes[0].Unlock()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/someorg/somerepo:Q4_K_M"}`))

	if w.Result().StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 (unresolvable model, healthy headroom must dispatch), got %d; body: %s", w.Result().StatusCode, w.Body.String())
	}
	waitForJob(t, s, "gpu-0", "hf.co/someorg/somerepo:Q4_K_M")
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

// TestHandleNodePull_DispatchesToAgentWhenCapable verifies marbor routes a
// pull to the node's Marbor Agent (not the direct Ollama /api/pull path) when
// the node has an agent enabled and reporting "models.pull" - and that the
// marbor's configured Hugging Face token is forwarded per-request (marbor-agent
// spec section 16). The direct-to-Ollama mock is never hit in this
// scenario, proving dispatch actually took the agent branch.
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
		if r.URL.Path != "/v1/models" || r.Method != http.MethodPost {
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
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetMarborAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentCapabilities = []string{"status", "models.pull"}
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
		t.Errorf("expected marbor's configured HF token forwarded to agent, got body %q", gotBody)
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

// TestHandleListActivePulls_ContinuityRestoresInFlightJobsAfterRefresh guards
// the continuity-bug class: GET /admin/pulls (added in
// f8d8049) must list a still-downloading job so PullProgressWidget.tsx can
// resubscribe on mount after a browser refresh, instead of losing all
// progress state because the widget's old in-memory-only tracking had
// nothing server-side to restore from. Also verifies a finished job drops off
// the active list, since the endpoint only returns status=="downloading".
func TestHandleListActivePulls_ContinuityRestoresInFlightJobsAfterRefresh(t *testing.T) {
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

	listReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/admin/pulls", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		return req
	}

	// Simulates a browser refresh mid-download: the widget resubscribes on
	// mount by calling this endpoint, and the still-downloading job must be
	// there for it to find.
	wList := httptest.NewRecorder()
	s.Handler().ServeHTTP(wList, listReq())
	if wList.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", wList.Result().StatusCode)
	}
	var jobs []pullJobSnapshot
	if err := json.NewDecoder(wList.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, j := range jobs {
		if j.Node == "gpu-0" && j.Model == "llama3:8b" {
			found = true
			if j.Status != "downloading" {
				t.Errorf("expected status=downloading, got %q", j.Status)
			}
		}
	}
	if !found {
		t.Fatal("expected the in-flight pull job to appear in GET /admin/pulls")
	}

	close(release)
	final := waitForJob(t, s, "gpu-0", "llama3:8b")
	if final.Status != "success" {
		t.Fatalf("expected job to finish success, got %q", final.Status)
	}

	wList2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(wList2, listReq())
	var jobs2 []pullJobSnapshot
	if err := json.NewDecoder(wList2.Body).Decode(&jobs2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, j := range jobs2 {
		if j.Node == "gpu-0" && j.Model == "llama3:8b" {
			t.Error("a finished job must not still appear in GET /admin/pulls")
		}
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

// newVerifyLoadTestServer builds a Server with one node (mockOllamaURL,
// standing in for the node's own Ollama API - handles /api/pull) and a proxy
// port pointed at mockProxyURL (standing in for the marbor's own
// /v1/chat/completions - what verifyModelLoads actually probes, via
// bench.MeasureChatTTFT), mirroring newBenchTestServer's setup in
// benchmark_test.go since this is the same underlying probe.
func newVerifyLoadTestServer(t *testing.T, mockOllamaURL, mockProxyURL string) *Server {
	t.Helper()
	cfg := config.Config{Auth: config.AuthConfig{Enabled: config.BoolPtr(true)}}
	u, err := url.Parse(mockProxyURL)
	if err != nil {
		t.Fatalf("parse mock proxy url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("mock proxy url has no numeric port: %v", err)
	}
	cfg.Proxy.Port = port
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllamaURL},
	}, nil)
	return NewServer(r, nil, cfg)
}

// TestHandleNodePull_VerifyLoadSucceeds verifies that an opt-in
// ("verify_load":true) pull runs a real load-verification probe after the
// download succeeds, and only then reports "success" - guarding the design
// intent that a bare download success is never conflated with "this model
// actually works here."
func TestHandleNodePull_VerifyLoadSucceeds(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mockProxy.Close()

	s := newVerifyLoadTestServer(t, mockOllama.URL, mockProxy.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b","verify_load":true}`))

	final := waitForJob(t, s, "gpu-0", "llama3:8b")
	if final.Status != "success" {
		t.Fatalf("expected verified pull to finish success, got %q (err=%q)", final.Status, final.Error)
	}
}

// TestHandleNodePull_VerifyLoadCatchesUnloadableModel is the regression test
// for the actual bug this feature closes: a model (e.g. a community
// Hugging Face GGUF whose declared architecture this node's Ollama can't
// load) downloads fine but fails the load-verification probe. The job must
// report a distinct "load_failed" status - not "success" - with Ollama's
// real error surfaced, so a user finds out at pull time instead of the next
// time something tries to actually use the model.
func TestHandleNodePull_VerifyLoadCatchesUnloadableModel(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"unable to load model: /usr/share/ollama/.ollama/models/blobs/sha256-abc123"}}`))
	}))
	defer mockProxy.Close()

	s := newVerifyLoadTestServer(t, mockOllama.URL, mockProxy.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"hf.co/yuxinlu1/broken-model:BF16","verify_load":true}`))

	final := waitForJob(t, s, "gpu-0", "hf.co/yuxinlu1/broken-model:BF16")
	if final.Status != "load_failed" {
		t.Fatalf("expected load_failed for an unloadable model, got %q", final.Status)
	}
	if !strings.Contains(final.Error, "unable to load model") {
		t.Errorf("expected the real Ollama error surfaced in Error, got %q", final.Error)
	}
}

// TestHandleNodePull_NoVerifyLoadSkipsProbe verifies the default
// (verify_load omitted/false) behavior is completely unchanged: no probe
// runs, and a download success is reported as "success" immediately, exactly
// as it always has been. A mock proxy that would fail any request it
// received guards against verifyModelLoads running when it shouldn't.
func TestHandleNodePull_NoVerifyLoadSkipsProbe(t *testing.T) {
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	probeCalled := false
	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalled = true
		http.Error(w, "should never be called", http.StatusInternalServerError)
	}))
	defer mockProxy.Close()

	s := newVerifyLoadTestServer(t, mockOllama.URL, mockProxy.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newPullRequest(t, s, "gpu-0", `{"model":"llama3:8b"}`))

	final := waitForJob(t, s, "gpu-0", "llama3:8b")
	if final.Status != "success" {
		t.Fatalf("expected success, got %q (err=%q)", final.Status, final.Error)
	}
	if probeCalled {
		t.Error("verifyModelLoads must not run when verify_load was not requested")
	}
}
