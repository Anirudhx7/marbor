package proxy

// Tests for the stalled-generation watchdog. Fake upstreams send headers and
// then go silent, or never answer, and the tests check what the router
// recorded and that the client stream was never touched. Deadlines are tiny
// and injected, so nothing here waits on the production floor.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

const stallChunk = `{"model":"llama3","response":"hi","done":false}` + "\n"
const stallDone = `{"done":true,"eval_count":1,"prompt_eval_count":1}` + "\n"

type stallEnv struct {
	h    *Handler
	node *router.NodeState
}

// newStallEnv builds a handler over one node. warm controls whether llama3 is
// loaded on it; deadline is the injected first-byte deadline; upstreamMs is
// the router's header timeout in milliseconds (0 = default).
func newStallEnv(t *testing.T, nodeURL string, warm bool, deadline time.Duration, upstreamMs int) *stallEnv {
	t.Helper()
	r := router.New(config.RoutingConfig{
		Strategy: "warm-first", Fallback: "least-connections", UpstreamTimeoutMs: upstreamMs,
	}, []config.NodeConfig{
		{Name: "mock-node", URL: nodeURL, GPUModel: "test-gpu", Runtime: "ollama"},
	}, nil)
	n := r.Nodes()[0]
	n.Lock()
	n.Healthy = true
	if warm {
		n.LoadedModels = []router.ModelInfo{{Name: "llama3"}}
	}
	n.Unlock()
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	h.stallDeadlineFor = func(*router.NodeState, int) time.Duration { return deadline }
	return &stallEnv{h: h, node: n}
}

func (e *stallEnv) stallCount() int {
	e.node.RLock()
	defer e.node.RUnlock()
	_, c, _, _ := e.node.StallViewLocked(time.Now())
	return c
}

func (e *stallEnv) post(path, body string) *streamRecorder {
	rec := newStreamRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

// slowUpstream answers headers immediately (flushed), then waits, then sends
// the body.
func slowUpstream(t *testing.T, afterHeaders time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(afterHeaders)
		io.WriteString(w, stallChunk+stallDone)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hangingUpstream sends nothing, not even headers, for the given time.
func hangingUpstream(t *testing.T, hang time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(hang):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, stallChunk+stallDone)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fastUpstream(t *testing.T) *httptest.Server {
	return slowUpstream(t, 0)
}

const genBody = `{"model":"llama3"}`

func TestStallWatch_HeadersThenSilentIsRecordedAndStreamNotAborted(t *testing.T) {
	srv := slowUpstream(t, 300*time.Millisecond)
	e := newStallEnv(t, srv.URL, true, 50*time.Millisecond, 0)
	rec := e.post("/api/generate", genBody)
	if got, want := rec.buf.String(), stallChunk+stallDone; got != want {
		t.Fatalf("client body = %q, want the full stream %q", got, want)
	}
	if c := e.stallCount(); c != 1 {
		t.Fatalf("stall count = %d, want 1", c)
	}
	// A stalled-then-finished stream does not clear the record, and three
	// distinct stalled requests flag the node.
	e.post("/api/generate", genBody)
	e.post("/api/generate", genBody)
	if !e.h.router.IsStalled("mock-node") {
		t.Fatalf("node not stalled after 3 stalled requests, count=%d", e.stallCount())
	}
}

func TestStallWatch_NoFirstByteByDeadlineIsRecorded(t *testing.T) {
	srv := hangingUpstream(t, 300*time.Millisecond)
	e := newStallEnv(t, srv.URL, true, 50*time.Millisecond, 0)
	rec := e.post("/api/generate", genBody)
	if !strings.Contains(rec.buf.String(), "hi") {
		t.Fatalf("the late answer must still reach the client, got %q", rec.buf.String())
	}
	if c := e.stallCount(); c != 1 {
		t.Fatalf("stall count = %d, want 1", c)
	}
}

func TestStallWatch_HeaderTimeoutCountsOnceEvenWhenTimerFiredFirst(t *testing.T) {
	srv := hangingUpstream(t, 2*time.Second)
	// Timer deadline 40ms, header timeout 120ms: both fire for one request.
	e := newStallEnv(t, srv.URL, true, 40*time.Millisecond, 120)
	e.post("/api/generate", genBody)
	if c := e.stallCount(); c != 1 {
		t.Fatalf("stall count = %d, want exactly 1 for one request", c)
	}
}

func TestStallWatch_HeaderTimeoutAloneIsRecorded(t *testing.T) {
	srv := hangingUpstream(t, 2*time.Second)
	// Timer deadline far away: only the transport header timeout fires.
	e := newStallEnv(t, srv.URL, true, time.Minute, 100)
	e.post("/api/generate", genBody)
	if c := e.stallCount(); c != 1 {
		t.Fatalf("stall count = %d, want 1 from the header timeout", c)
	}
}

func TestStallWatch_SlowButWithinDeadlineIsNotFlagged(t *testing.T) {
	srv := slowUpstream(t, 80*time.Millisecond)
	e := newStallEnv(t, srv.URL, true, 2*time.Second, 0)
	e.post("/api/generate", genBody)
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0 for a slow stream inside its deadline", c)
	}
}

func TestStallWatch_NonStreamingIsNeverJudged(t *testing.T) {
	srv := hangingUpstream(t, 250*time.Millisecond)
	e := newStallEnv(t, srv.URL, true, 40*time.Millisecond, 0)
	e.post("/v1/chat/completions", `{"model":"llama3","messages":[]}`)
	e.post("/api/generate", `{"model":"llama3","stream":false}`)
	e.post("/v1/chat/completions", `{"model":"llama3","stream":false}`)
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0 for non-streaming requests", c)
	}
}

func TestStallWatch_NonStreamHeaderTimeoutIsNotCounted(t *testing.T) {
	srv := hangingUpstream(t, 2*time.Second)
	e := newStallEnv(t, srv.URL, true, time.Minute, 100)
	e.post("/v1/chat/completions", `{"model":"llama3","messages":[]}`)
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0: a non-stream header timeout is no verdict", c)
	}
}

func TestStallWatch_ColdModelIsNotJudged(t *testing.T) {
	srv := hangingUpstream(t, 250*time.Millisecond)
	e := newStallEnv(t, srv.URL, false, 40*time.Millisecond, 0)
	e.post("/api/generate", genBody)
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0 for a model that was not warm at dispatch", c)
	}
}

func TestStallWatch_CleanFastStreamClearsRecord(t *testing.T) {
	srv := fastUpstream(t)
	e := newStallEnv(t, srv.URL, true, 2*time.Second, 0)
	e.h.router.RecordStall("mock-node", router.StallPhaseFirstByte, "old-1")
	e.h.router.RecordStall("mock-node", router.StallPhaseFirstByte, "old-2")
	e.h.router.RecordStall("mock-node", router.StallPhaseFirstByte, "old-3")
	if !e.h.router.IsStalled("mock-node") {
		t.Fatal("precondition: node should be stalled")
	}
	e.post("/api/generate", genBody)
	if e.h.router.IsStalled("mock-node") || e.stallCount() != 0 {
		t.Fatalf("a clean stream must clear the record, count=%d", e.stallCount())
	}
}

func TestStallWatch_NonStreamSuccessDoesNotClear(t *testing.T) {
	srv := fastUpstream(t)
	e := newStallEnv(t, srv.URL, true, 2*time.Second, 0)
	e.h.router.RecordStall("mock-node", router.StallPhaseFirstByte, "old-1")
	e.post("/v1/chat/completions", `{"model":"llama3","messages":[]}`)
	if c := e.stallCount(); c != 1 {
		t.Fatalf("a non-stream success is no evidence either way, count=%d want 1", c)
	}
}

func TestStallWatch_ClientDisconnectIsNotCounted(t *testing.T) {
	arrived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	e := newStallEnv(t, srv.URL, true, 150*time.Millisecond, 0)
	front := httptest.NewServer(e.h)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/api/generate", strings.NewReader(genBody))
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the upstream")
	}
	cancel()
	<-done
	time.Sleep(400 * time.Millisecond) // well past the 150ms deadline
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0 after a client disconnect", c)
	}
}

func stallsOn(node *router.NodeState) int {
	node.RLock()
	defer node.RUnlock()
	return node.ConsecutiveStalls
}

// waitFor polls cond until it holds or the limit passes.
func waitFor(t *testing.T, cond func() bool, limit time.Duration) {
	t.Helper()
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func newTestWatchParts(t *testing.T) (*Handler, *router.NodeState, *statusRecorder) {
	t.Helper()
	e := newStallEnv(t, "http://127.0.0.1:1", true, time.Minute, 0)
	return e.h, e.node, &statusRecorder{ResponseWriter: httptest.NewRecorder()}
}

func TestStallWatch_LateCallbackAfterCompletionRecordsNothing(t *testing.T) {
	h, node, rec := newTestWatchParts(t)
	w := h.startStallWatch(context.Background(), rec, node, "req-1", 20*time.Millisecond)
	// Completion wins the race, as when the request ends right at the deadline
	// and Stop arrives too late to cancel an already-fired timer.
	w.state.Store(stallWatchDone)
	time.Sleep(300 * time.Millisecond)
	if h.router.IsStalled("mock-node") {
		t.Fatal("late timer callback must not record after completion")
	}
	node.RLock()
	c := node.ConsecutiveStalls
	node.RUnlock()
	if c != 0 {
		t.Fatalf("ConsecutiveStalls = %d, want 0", c)
	}
}

// A timer that is allowed to fire records the stall, so the late-callback test
// above is exercising a real armed timer and not an absent one.
func TestStallWatch_ArmedTimerFiresAndRecords(t *testing.T) {
	h, node, rec := newTestWatchParts(t)
	w := h.startStallWatch(context.Background(), rec, node, "req-1", 10*time.Millisecond)
	waitFor(t, func() bool { return stallsOn(node) == 1 }, 5*time.Second)
	if w.finish() {
		t.Fatal("finish after the timer won must not report clean")
	}
}

func TestStallWatch_FinishBeforeDeadlineStopsTheTimer(t *testing.T) {
	h, node, rec := newTestWatchParts(t)
	w := h.startStallWatch(context.Background(), rec, node, "req-1", 30*time.Millisecond)
	if !w.finish() {
		t.Fatal("finish before the deadline must report clean")
	}
	time.Sleep(300 * time.Millisecond)
	node.RLock()
	c := node.ConsecutiveStalls
	node.RUnlock()
	if c != 0 {
		t.Fatalf("ConsecutiveStalls = %d, want 0", c)
	}
}

func TestStallWatch_FinishAfterDeadlineIsNotClean(t *testing.T) {
	h, node, rec := newTestWatchParts(t)
	w := h.startStallWatch(context.Background(), rec, node, "req-1", 10*time.Millisecond)
	waitFor(t, func() bool { return stallsOn(node) == 1 }, 5*time.Second)
	if w.finish() {
		t.Fatal("a watch that crossed its deadline must not report clean")
	}
}

func TestStallWatch_NilWatchIsSafe(t *testing.T) {
	var w *stallWatch
	if w.finish() || w.markStalled() {
		t.Fatal("nil watch must report false")
	}
}

func TestStallWatch_ByteSeenSuppressesTimer(t *testing.T) {
	h, node, rec := newTestWatchParts(t)
	if _, err := rec.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	w := h.startStallWatch(context.Background(), rec, node, "req-1", 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	node.RLock()
	c := node.ConsecutiveStalls
	node.RUnlock()
	if c != 0 {
		t.Fatalf("a request that produced bytes must not count, got %d", c)
	}
	w.finish()
}

func TestStatusRecorder_ConcurrentWriteAndByteProbeIsRaceFree(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			rec.Write([]byte("a\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = rec.sawAnyByte()
		}
	}()
	wg.Wait()
	if !rec.sawAnyByte() {
		t.Fatal("bytes were written")
	}
}

func TestStatusRecorder_StillAFlusher(t *testing.T) {
	var _ http.Flusher = (*statusRecorder)(nil)
}

func TestStallWatch_StreamStillArrivesIncrementallyWithWatchArmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, stallChunk)
		f.Flush()
		time.Sleep(80 * time.Millisecond)
		io.WriteString(w, stallDone)
		f.Flush()
	}))
	defer srv.Close()
	e := newStallEnv(t, srv.URL, true, 2*time.Second, 0)
	rec := e.post("/api/generate", genBody)
	if len(rec.writeTimes) < 2 {
		t.Fatalf("got %d writes, want >= 2 (stream must not be buffered)", len(rec.writeTimes))
	}
	if gap := rec.writeTimes[len(rec.writeTimes)-1].Sub(rec.writeTimes[0]); gap < 60*time.Millisecond {
		t.Fatalf("chunks arrived together (gap %v): response was buffered", gap)
	}
	if rec.flushes == 0 {
		t.Fatal("Flush never reached the client")
	}
}

func TestIsStreamingRequest(t *testing.T) {
	cases := []struct {
		path, body string
		want       bool
	}{
		{"/api/generate", `{"model":"m"}`, true},
		{"/api/chat", `{"model":"m"}`, true},
		{"/api/generate", `{"model":"m","stream":false}`, false},
		{"/api/chat", `not json`, true},
		{"/v1/chat/completions", `{"model":"m"}`, false},
		{"/v1/chat/completions", `{"model":"m","stream":true}`, true},
		{"/v1/completions", `{"model":"m","stream":false}`, false},
		{"/v1/embeddings", `{"model":"m"}`, false},
		{"/v1/chat/completions", `garbage`, false},
	}
	for _, c := range cases {
		if got := isStreamingRequest(c.path, []byte(c.body)); got != c.want {
			t.Errorf("isStreamingRequest(%q, %s) = %v, want %v", c.path, c.body, got, c.want)
		}
	}
}

type dialTimeoutErr struct{}

func (dialTimeoutErr) Error() string   { return "i/o timeout" }
func (dialTimeoutErr) Timeout() bool   { return true }
func (dialTimeoutErr) Temporary() bool { return true }

func TestStallWatch_DialTimeoutIsNotAStall(t *testing.T) {
	e := newStallEnv(t, "http://127.0.0.1:1", true, time.Minute, 0)
	e.h.localTransportOnce.Do(func() {
		e.h.localTransport = &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: dialTimeoutErr{}}
			},
		}
	})
	e.post("/api/generate", genBody)
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0: an unreachable node is not a stalled generation", c)
	}
}

func TestStallWatch_RefusedConnectionIsNotAStall(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	e := newStallEnv(t, url, true, time.Minute, 0)
	e.post("/api/generate", genBody)
	if c := e.stallCount(); c != 0 {
		t.Fatalf("stall count = %d, want 0 for a refused connection", c)
	}
}

// The byte check and the move to stalled are one step, so a finish() that
// arrives mid-decision waits and then sees a consistent result.
func TestStallWatch_FinishDuringTimerDecisionIsConsistent(t *testing.T) {
	for _, byteArrives := range []bool{false, true} {
		h, node, rec := newTestWatchParts(t)
		inside := make(chan struct{})
		release := make(chan struct{})
		w := &stallWatch{}
		w.byteSeen = func() bool {
			close(inside)
			<-release
			return byteArrives
		}
		w.arm(h, context.Background(), rec, node, "req-1", time.Millisecond)
		<-inside
		got := make(chan bool, 1)
		go func() { got <- w.finish() }()
		time.Sleep(50 * time.Millisecond) // finish is now waiting on the decision
		close(release)
		clean := <-got
		stalls := 0
		waitFor(t, func() bool { stalls = stallsOn(node); return stalls == boolToInt(!byteArrives) }, 5*time.Second)
		if clean != byteArrives {
			t.Fatalf("byteArrives=%v: finish clean=%v, want %v", byteArrives, clean, byteArrives)
		}
		if w.state.Load() == stallWatchRunning {
			t.Fatalf("byteArrives=%v: watch left running", byteArrives)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Two attempts: the first node refuses the dial (no stall, and not marked
// connected), the second times out waiting for headers and is the only one
// counted.
func TestStallWatch_RetryDialFailureThenHeaderTimeoutCountsSecondNodeOnly(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	hung := hangingUpstream(t, 2*time.Second)
	r := router.New(config.RoutingConfig{
		Strategy: "warm-first", Fallback: "least-connections", UpstreamTimeoutMs: 100, MaxRetries: 1,
	}, []config.NodeConfig{
		{Name: "dead-node", URL: deadURL, GPUModel: "test-gpu", Runtime: "ollama"},
		{Name: "hung-node", URL: hung.URL, GPUModel: "test-gpu", Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.LoadedModels = []router.ModelInfo{{Name: "llama3"}}
		n.Unlock()
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	h.stallDeadlineFor = func(*router.NodeState, int) time.Duration { return time.Minute }
	rec := newStreamRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(genBody)))
	counts := map[string]int{}
	for _, n := range r.Nodes() {
		counts[n.Name] = stallsOn(n)
	}
	if counts["dead-node"] != 0 {
		t.Fatalf("dead node stalls = %d, want 0 (dial failure)", counts["dead-node"])
	}
	// Whichever node was tried first, a node that timed out after connecting
	// is counted and a refused one never is.
	if counts["hung-node"] != 1 {
		t.Fatalf("hung node stalls = %d, want 1 (counts=%v)", counts["hung-node"], counts)
	}
}

// A header timeout on a reused (kept-alive) connection still counts: the
// connection exists, so the silence is the node's.
func TestStallWatch_HeaderTimeoutOnReusedConnectionCounts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			io.WriteString(w, stallChunk+stallDone)
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	e := newStallEnv(t, srv.URL, true, time.Minute, 100)
	e.post("/api/generate", genBody) // opens and parks a keep-alive connection
	if c := e.stallCount(); c != 0 {
		t.Fatalf("clean first request stalls = %d, want 0", c)
	}
	e.post("/api/generate", genBody) // reuses it, then times out on headers
	if c := e.stallCount(); c != 1 {
		t.Fatalf("stall count = %d, want 1 for a timeout on a reused connection", c)
	}
}
