package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// newBenchTestServer builds a Server with one node (mockNodeURL, standing in
// for the node's own Ollama API - used only for the eviction call) and a
// proxy port pointed at mockProxyURL (standing in for the mesh's own
// /v1/chat/completions endpoint - what the benchmark actually measures).
// model is pre-registered as already loaded on the node, matching what
// nodeHasModel's LoadedModels fallback checks, so tests don't need to mock
// a Node Agent just to pass the pre-flight model-known check.
func newBenchTestServer(t *testing.T, mockNodeURL, mockProxyURL, model string) *Server {
	t.Helper()
	cfg := config.Config{
		Auth: config.AuthConfig{Enabled: config.BoolPtr(true)},
	}
	if mockProxyURL != "" {
		u, err := url.Parse(mockProxyURL)
		if err != nil {
			t.Fatalf("parse mock proxy url: %v", err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatalf("mock proxy url has no numeric port: %v", err)
		}
		cfg.Proxy.Port = port
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: mockNodeURL},
	}, nil)
	// A real SQLite store (not NopStore) is required here: this feature's
	// whole point is persisting benchmark_runs, and a NopStore would silently
	// swallow InsertBenchmarkRun/ListBenchmarkRuns without ever surfacing that
	// as a test failure.
	tmpDB := filepath.Join(t.TempDir(), "benchmark-test.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(r, nil, cfg, st)
	if model != "" {
		for _, n := range s.router.Nodes() {
			n.Lock()
			n.LoadedModels = append(n.LoadedModels, router.ModelInfo{Name: model})
			n.Unlock()
		}
	}
	return s
}

func newBenchRunRequest(t *testing.T, s *Server, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/benchmark/run", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	return req
}

// sseChatCompletionsHandler answers /v1/chat/completions with a single-chunk
// SSE stream carrying one non-empty token, matching what
// bench.MeasureChatTTFT expects to see to record a TTFT sample. Every
// request's Authorization header is recorded so tests can assert the
// ephemeral key actually reached the wire.
func sseChatCompletionsHandler(seenKeys *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if seenKeys != nil {
			*seenKeys = append(*seenKeys, r.Header.Get("Authorization"))
		}
		// A small floor so the TTFT sample is never a flaky 0ms on a fast
		// loopback round-trip - this test cares about orchestration
		// correctness (samples recorded, run persisted), not real timing.
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// waitForBenchJob polls s.benchJobs directly until the job reaches a
// terminal phase or the timeout elapses, mirroring pull_test.go's
// waitForJob for the same asynchronous-handler reason.
func waitForBenchJob(t *testing.T, s *Server, jobID string) benchmarkJobSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.benchMu.Lock()
		job, ok := s.benchJobs[jobID]
		s.benchMu.Unlock()
		if ok {
			snap := job.snapshot()
			switch snap.Phase {
			case "done", "error", "cancelled":
				return snap
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %q did not reach a terminal phase within timeout", jobID)
	return benchmarkJobSnapshot{}
}

func runBenchmarkRequest(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newBenchRunRequest(t, s, body))
	res := w.Result()
	if res.StatusCode != http.StatusAccepted {
		return res.StatusCode, ""
	}
	var resp map[string]string
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	return res.StatusCode, resp["job_id"]
}

// assertKeyFullyDeleted polls (up to 2s, since cleanup runs in the job
// goroutine's defer, racing the test) until the ephemeral key named with the
// "benchmark-<node>-<model>-" prefix is absent from both the store's
// persisted key list and the in-memory auth map's key names - the two
// places a stale ephemeral key could otherwise still authenticate from.
func assertKeyFullyDeleted(t *testing.T, s *Server, node, model, keyValue string) {
	t.Helper()
	prefix := fmt.Sprintf("benchmark-%s-%s-", node, model)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inStore := false
		if recs, err := s.st.AllKeys(); err == nil {
			for _, rec := range recs {
				if rec.Key == keyValue || strings.HasPrefix(rec.Name, prefix) {
					inStore = true
					break
				}
			}
		}
		inAuth := false
		if s.auth != nil {
			for _, name := range s.auth.AllKeyNames() {
				if strings.HasPrefix(name, prefix) {
					inAuth = true
					break
				}
			}
		}
		if !inStore && !inAuth {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected ephemeral key (prefix %q) to be fully deleted from store and auth", prefix)
}

func TestHandleRunBenchmark_ValidationErrors(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "", "llama3:8b")

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing node/model", `{}`, http.StatusBadRequest},
		{"n too large", `{"node":"gpu-0","model":"llama3:8b","n":51}`, http.StatusBadRequest},
		{"node not found", `{"node":"does-not-exist","model":"llama3:8b"}`, http.StatusNotFound},
		{"model not known on node", `{"node":"gpu-0","model":"unknown-model"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, newBenchRunRequest(t, s, tc.body))
			if w.Result().StatusCode != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, w.Result().StatusCode)
			}
		})
	}
}

func TestHandleRunBenchmark_DownNodeFailsFast(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "", "llama3:8b")
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = false
	nodes[0].Unlock()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newBenchRunRequest(t, s, `{"node":"gpu-0","model":"llama3:8b"}`))
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Result().StatusCode)
	}
}

// TestHandleRunBenchmark_SuccessCleansUpEphemeralKey drives one full n=1
// cold+warm run to completion and verifies: (1) the run landed in
// benchmark_runs with a real speedup figure, (2) the ephemeral API key used
// on the wire is gone from both the in-memory auth map and the store
// immediately after the job finishes - the central safety property of the
// whole feature (no orphaned key survives a successful run).
func TestHandleRunBenchmark_SuccessCleansUpEphemeralKey(t *testing.T) {
	mockNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockNode.Close()

	var seenKeys []string
	mockProxy := httptest.NewServer(sseChatCompletionsHandler(&seenKeys))
	defer mockProxy.Close()

	s := newBenchTestServer(t, mockNode.URL, mockProxy.URL, "llama3:8b")

	status, jobID := runBenchmarkRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":1}`)
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", status)
	}

	final := waitForBenchJob(t, s, jobID)
	if final.Phase != "done" {
		t.Fatalf("expected phase=done, got %q (err=%q)", final.Phase, final.Error)
	}
	if final.Result == nil {
		t.Fatalf("expected a non-nil result on a done job")
	}
	if final.Result.SpeedupX <= 0 {
		t.Errorf("expected a positive speedup, got %v", final.Result.SpeedupX)
	}

	runs, err := s.st.ListBenchmarkRuns(10)
	if err != nil {
		t.Fatalf("ListBenchmarkRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 persisted run, got %d", len(runs))
	}
	if runs[0].Node != "gpu-0" || runs[0].Model != "llama3:8b" {
		t.Fatalf("unexpected persisted run: %+v", runs[0])
	}

	if len(seenKeys) == 0 {
		t.Fatalf("expected at least one request to reach the mock proxy")
	}
	usedKey := strings.TrimPrefix(seenKeys[0], "Bearer ")
	if usedKey == "" {
		t.Fatalf("expected a real bearer key on the wire, got empty")
	}

	// The key must be gone - not merely revoked - from both places a stale
	// entry could otherwise still authenticate from.
	assertKeyFullyDeleted(t, s, "gpu-0", "llama3:8b", usedKey)
}

// TestHandleRunBenchmark_ErrorPathCleansUpEphemeralKey verifies the same
// ephemeral-key cleanup happens when a sample fails outright (mock proxy
// answers every request with 500), not just on the happy path.
func TestHandleRunBenchmark_ErrorPathCleansUpEphemeralKey(t *testing.T) {
	mockNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockNode.Close()

	var seenKeys []string
	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKeys = append(seenKeys, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockProxy.Close()

	s := newBenchTestServer(t, mockNode.URL, mockProxy.URL, "llama3:8b")

	status, jobID := runBenchmarkRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":1}`)
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", status)
	}

	final := waitForBenchJob(t, s, jobID)
	if final.Phase != "error" {
		t.Fatalf("expected phase=error, got %q", final.Phase)
	}
	if final.Error == "" {
		t.Fatalf("expected a non-empty error message")
	}

	if len(seenKeys) == 0 || seenKeys[0] == "" {
		t.Fatalf("expected the ephemeral key to have reached the mock proxy before failing")
	}
	usedKey := strings.TrimPrefix(seenKeys[0], "Bearer ")
	assertKeyFullyDeleted(t, s, "gpu-0", "llama3:8b", usedKey)
}

// TestHandleCancelBenchmark_StopsAnInFlightRunAndCleansUpKey verifies
// cancellation both interrupts the run and still runs the deferred
// ephemeral-key cleanup - the cancel path must not leak a key any more than
// the success or error paths do.
func TestHandleCancelBenchmark_StopsAnInFlightRunAndCleansUpKey(t *testing.T) {
	mockNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockNode.Close()

	release := make(chan struct{})
	var usedKeyMu sync.Mutex
	var usedKey string
	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usedKeyMu.Lock()
		usedKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		usedKeyMu.Unlock()
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer mockProxy.Close()

	s := newBenchTestServer(t, mockNode.URL, mockProxy.URL, "llama3:8b")

	status, jobID := runBenchmarkRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":5}`)
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", status)
	}

	// Wait for the job to actually reach the mock proxy (first sample in
	// flight) before cancelling, so this exercises a real mid-run cancel.
	// Generous deadline: runBenchmarkJob sleeps 2s after the initial evict
	// before firing the first cold sample.
	deadline := time.Now().Add(5 * time.Second)
	getUsedKey := func() string {
		usedKeyMu.Lock()
		defer usedKeyMu.Unlock()
		return usedKey
	}
	for getUsedKey() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if getUsedKey() == "" {
		t.Fatalf("job never reached the mock proxy")
	}

	cancelReq := httptest.NewRequest(http.MethodDelete, "/admin/benchmark/"+jobID, nil)
	cancelReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	cancelReq.SetPathValue("id", jobID)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, cancelReq)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from cancel, got %d", w.Result().StatusCode)
	}

	close(release)

	final := waitForBenchJob(t, s, jobID)
	if final.Phase != "cancelled" {
		t.Fatalf("expected phase=cancelled, got %q", final.Phase)
	}

	assertKeyFullyDeleted(t, s, "gpu-0", "llama3:8b", getUsedKey())
}

// TestHandleCancelBenchmark_AbortsInFlightSamplePromptly regression-tests the
// fix threading ctx into bench.MeasureChatTTFT: before that fix, the job
// goroutine's ctx.Err() check only ran BETWEEN samples, so cancelling while a
// sample's client.Do was blocked (e.g. a slow cold load) left that request -
// and the ephemeral key's cleanup, which only runs after runBenchmarkJob
// returns - waiting out the full benchmarkSampleTimeout (5 minutes) instead
// of aborting immediately.
//
// job.requestCancel() flips Phase to "cancelled" synchronously (see
// benchmark.go), before the underlying ctx.CancelFunc is even invoked - so
// asserting Phase alone would pass regardless of whether the fix works. The
// real signal is the ephemeral key's deletion, which is gated behind
// runBenchmarkJob's deferred cleanup and therefore behind client.Do actually
// returning. The mock proxy never answers on its own (unlike the sibling
// test above, which manually releases it), so a prompt key deletion here can
// only mean the outbound request was actually torn down by ctx cancellation.
//
// The handler's own wait is bounded by a generous backstop timer (not tied
// to the behavior under test) purely so a regression can never hang this
// suite - httptest.Server.Close() will not return until the handler
// goroutine does, with no way to force it from outside.
func TestHandleCancelBenchmark_AbortsInFlightSamplePromptly(t *testing.T) {
	mockNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockNode.Close()

	var usedKeyMu sync.Mutex
	var usedKey string
	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usedKeyMu.Lock()
		usedKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		usedKeyMu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	defer mockProxy.Close()

	s := newBenchTestServer(t, mockNode.URL, mockProxy.URL, "llama3:8b")

	status, jobID := runBenchmarkRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":5}`)
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", status)
	}

	getUsedKey := func() string {
		usedKeyMu.Lock()
		defer usedKeyMu.Unlock()
		return usedKey
	}
	deadline := time.Now().Add(5 * time.Second)
	for getUsedKey() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if getUsedKey() == "" {
		t.Fatalf("job never reached the mock proxy")
	}

	cancelReq := httptest.NewRequest(http.MethodDelete, "/admin/benchmark/"+jobID, nil)
	cancelReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	cancelReq.SetPathValue("id", jobID)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, cancelReq)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from cancel, got %d", w.Result().StatusCode)
	}

	final := waitForBenchJob(t, s, jobID)
	if final.Phase != "cancelled" {
		t.Fatalf("expected phase=cancelled, got %q", final.Phase)
	}

	// The real assertion: if ctx cancellation didn't abort client.Do, this
	// key would still be live for up to 20s (the mock's backstop) - well
	// past assertKeyFullyDeleted's 2s deadline - failing the test instead of
	// masking the bug behind Phase alone.
	assertKeyFullyDeleted(t, s, "gpu-0", "llama3:8b", getUsedKey())
}

// TestHandleRunBenchmark_RejectsConcurrentRunOnSameNode verifies the
// same-node in-flight guard: a second POST /admin/benchmark/run for a node
// that already has an active job must be rejected with 409, not allowed to
// race the first job's evict/reload cycle and corrupt both runs' numbers.
func TestHandleRunBenchmark_RejectsConcurrentRunOnSameNode(t *testing.T) {
	mockNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockNode.Close()

	block := make(chan struct{})
	defer close(block)
	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer mockProxy.Close()

	s := newBenchTestServer(t, mockNode.URL, mockProxy.URL, "llama3:8b")

	status1, jobID1 := runBenchmarkRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":5}`)
	if status1 != http.StatusAccepted {
		t.Fatalf("first run: expected 202, got %d", status1)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.benchMu.Lock()
		_, ok := s.benchJobs[jobID1]
		s.benchMu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	status2, _ := runBenchmarkRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":5}`)
	if status2 != http.StatusConflict {
		t.Fatalf("second concurrent run on the same node: expected 409, got %d", status2)
	}
}

func TestHandleListBenchmarkRuns_EmptyByDefault(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "", "")

	req := httptest.NewRequest(http.MethodGet, "/admin/benchmark/runs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var resp struct {
		Runs []interface{} `json:"runs"`
	}
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 0 {
		t.Fatalf("expected no runs, got %d", len(resp.Runs))
	}
}
