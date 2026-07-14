package proxy

// Streaming integration tests for the proxy hot path (rule R2: streaming must
// never be buffered). Each test stands up a mock Ollama/OpenAI node with
// httptest, drives the real Handler, and observes the client side through a
// write-time-recording ResponseWriter.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// newDyingNode returns a mock Ollama node that sends one chunk, then severs
// the connection mid-stream (no terminal chunk, no clean EOF).
func newDyingNode(t *testing.T, firstChunk string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, firstChunk)
		if f != nil {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("mock node ResponseWriter does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close() // abrupt close mid-stream
	}))
}

// streamRecorder is an http.ResponseWriter that records the timestamp of
// every Write call, so tests can prove chunks arrived incrementally rather
// than as one buffered blob. It implements http.Flusher because the proxy
// path requires a flushable writer for unbuffered streaming.
type streamRecorder struct {
	mu         sync.Mutex
	header     http.Header
	statusCode int
	buf        bytes.Buffer
	writeTimes []time.Time
	flushes    int
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header)}
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.statusCode == 0 {
		r.statusCode = code
	}
}

func (r *streamRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeTimes = append(r.writeTimes, time.Now())
	return r.buf.Write(b)
}

func (r *streamRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

// newStreamTestHandler wires a Handler to a single healthy node at nodeURL,
// mirroring how main.go constructs the proxy (router + admin, no audit).
func newStreamTestHandler(t *testing.T, nodeURL string) (*Handler, *admin.Server) {
	t.Helper()
	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"}, []config.NodeConfig{
		{Name: "mock-node", URL: nodeURL, GPUModel: "test-gpu", Runtime: "ollama"},
	}, nil)
	n := r.Nodes()[0]
	n.Lock()
	n.Healthy = true
	n.Unlock()
	a := admin.NewServer(r, nil, config.Config{})
	return NewHandler(r, a, nil), a
}

func TestStreamingUnbuffered(t *testing.T) {
	const interChunkDelay = 50 * time.Millisecond
	chunks := []string{
		`{"model":"llama3","response":"a","done":false}` + "\n",
		`{"model":"llama3","response":"b","done":false}` + "\n",
		`{"done":true,"eval_count":100,"prompt_eval_count":20}` + "\n",
	}

	var mockDoneNanos atomic.Int64 // when the mock finished sending all chunks
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock node ResponseWriter does not implement http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i, c := range chunks {
			if i > 0 {
				time.Sleep(interChunkDelay)
			}
			io.WriteString(w, c)
			f.Flush()
		}
		mockDoneNanos.Store(time.Now().UnixNano())
	}))
	defer node.Close()

	h, _ := newStreamTestHandler(t, node.URL)
	rec := newStreamRecorder()
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"llama3"}`))
	h.ServeHTTP(rec, req)

	if got, want := rec.buf.String(), strings.Join(chunks, ""); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if len(rec.writeTimes) < 2 {
		t.Fatalf("got %d Write calls, want >= 2 (response arrived as a single buffered write)", len(rec.writeTimes))
	}
	first := rec.writeTimes[0]
	last := rec.writeTimes[len(rec.writeTimes)-1]
	if gap := last.Sub(first); gap < interChunkDelay {
		t.Errorf("gap between first and last client write = %v, want >= %v (chunks should arrive incrementally)", gap, interChunkDelay)
	}
	mockDone := time.Unix(0, mockDoneNanos.Load())
	if !first.Before(mockDone) {
		t.Error("first client write happened after the mock finished sending all chunks - response was buffered")
	}
	if rec.flushes == 0 {
		t.Error("Flush never reached the client ResponseWriter (statusRecorder must forward Flush)")
	}
}

func TestStreamingTokensTracked(t *testing.T) {
	chunks := []string{
		`{"model":"llama3","response":"a","done":false}` + "\n",
		`{"model":"llama3","response":"b","done":false}` + "\n",
		`{"done":true,"eval_count":100,"prompt_eval_count":20}` + "\n",
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, c := range chunks {
			io.WriteString(w, c)
			if f != nil {
				f.Flush()
			}
		}
	}))
	defer node.Close()

	h, a := newStreamTestHandler(t, node.URL)
	rec := newStreamRecorder()
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"llama3"}`))
	h.ServeHTTP(rec, req)

	if got := a.LocalTokens(); got != 120 {
		t.Errorf("LocalTokens = %d, want 120 (eval_count 100 + prompt_eval_count 20)", got)
	}
}

func TestStreamingNodeDiesMidStream(t *testing.T) {
	firstChunk := `{"model":"llama3","response":"partial","done":false}` + "\n"
	node := newDyingNode(t, firstChunk)
	defer node.Close()

	h, _ := newStreamTestHandler(t, node.URL)

	// Run the Handler behind a real HTTP server. When the upstream dies
	// mid-body, httputil.ReverseProxy aborts with http.ErrAbortHandler,
	// which net/http recovers - exactly what happens in production. Calling
	// h.ServeHTTP directly would surface that abort as a test panic.
	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"llama3"}`))
	if err != nil {
		t.Fatalf("request failed before any response was delivered: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body) // read error expected: stream cut mid-body
	resp.Body.Close()
	if !strings.Contains(string(body), `"response":"partial"`) {
		t.Errorf("partial response not delivered to client: body = %q (read err: %v)", body, readErr)
	}

	// The proxy must survive the dead upstream: a follow-up request still
	// gets a response instead of a connection error.
	resp2, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"llama3"}`))
	if err != nil {
		t.Fatalf("proxy did not survive upstream dying mid-stream: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
}

// TestAbortedStreamStillRecorded proves the fix for the observability gap:
// when the upstream dies mid-stream, httputil.ReverseProxy panics with
// http.ErrAbortHandler. Before the fix, the net/http server recovered that
// panic above the handler and the request vanished from the admin log,
// audit log, and metrics. Now it must be recorded with status "aborted".
func TestAbortedStreamStillRecorded(t *testing.T) {
	firstChunk := `{"model":"llama3","response":"partial","done":false}` + "\n"
	node := newDyingNode(t, firstChunk)
	defer node.Close()

	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"}, []config.NodeConfig{
		{Name: "mock-node", URL: node.URL, GPUModel: "test-gpu", Runtime: "ollama"},
	}, nil)
	n := r.Nodes()[0]
	n.Lock()
	n.Healthy = true
	n.Unlock()
	a := admin.NewServer(r, nil, config.Config{})
	tmpDB := filepath.Join(t.TempDir(), "audit.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	al := audit.New(st, true)
	defer al.Close()
	h := NewHandler(r, a, al)

	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := http.Post(front.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"llama3"}`))
	if err != nil {
		t.Fatalf("request failed before any response was delivered: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck  --  truncated stream, read error possible
	resp.Body.Close()

	entries := fetchLiveRequests(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request log entries, want 1 (aborted request must still be logged)", len(entries))
	}
	if entries[0].Status != "aborted" {
		t.Errorf("request log status = %q, want aborted", entries[0].Status)
	}
	if entries[0].Model != "llama3" {
		t.Errorf("request log model = %q, want llama3", entries[0].Model)
	}

	audits, err := al.Query(audit.QueryOptions{})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("got %d audit entries, want 1 (aborted request must still be audited)", len(audits))
	}
	if audits[0].Status != "aborted" {
		t.Errorf("audit status = %q, want aborted", audits[0].Status)
	}
}

func TestSSEPassthrough(t *testing.T) {
	events := []string{
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":35,\"total_tokens\":42}}\n\n",
		"data: [DONE]\n\n",
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock node ResponseWriter does not implement http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, ev := range events {
			if i > 0 {
				time.Sleep(20 * time.Millisecond)
			}
			io.WriteString(w, ev)
			f.Flush()
		}
	}))
	defer node.Close()

	h, a := newStreamTestHandler(t, node.URL)
	rec := newStreamRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-style","stream":true}`))
	h.ServeHTTP(rec, req)

	if got, want := rec.buf.String(), strings.Join(events, ""); got != want {
		t.Errorf("SSE stream not passed through verbatim:\ngot  %q\nwant %q", got, want)
	}
	if ct := rec.header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if len(rec.writeTimes) < 2 {
		t.Errorf("got %d Write calls, want >= 2 (SSE events should stream incrementally)", len(rec.writeTimes))
	}
	if got := a.LocalTokens(); got != 42 {
		t.Errorf("LocalTokens = %d, want 42 (usage.total_tokens from final SSE chunk)", got)
	}
}
