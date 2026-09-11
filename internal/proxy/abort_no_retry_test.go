package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

// TestRetryNeverFiresAfterFirstResponseByte pins the guarantee investigated by
// P425's audit: once a backend has written response headers and body bytes,
// a mid-stream failure must never be retried against a second node. Today
// that guarantee is inherited from an undocumented stdlib detail -
// httputil.ReverseProxy calls ErrorHandler only for a pre-header RoundTrip
// error, and panics http.ErrAbortHandler on a post-header copy error instead
// (net/http/httputil/reverseproxy.go, copyResponse) - and our retry loop
// lives entirely inside ErrorHandler. This test asserts the OBSERVABLE
// contract, not that stdlib detail: one partial response reaches the client,
// the second node is never touched, and the outcome is recorded as a
// failure rather than a fabricated 200.
//
// The request must be routed through a real net/http.Server (httptest.
// NewServer), not a bare httptest.NewRequest + ServeHTTP call: stdlib's
// shouldPanicOnCopyError only panics when the incoming request's context
// carries http.ServerContextKey, which only a real server sets. Skipping
// this would silently test a path production traffic never takes.
func TestRetryNeverFiresAfterFirstResponseByte(t *testing.T) {
	var flakyHits, goodHits int32

	// The flaky node claims more bytes than it writes, so the connection
	// closes mid-body - the client sees a genuine copy error (truncated
	// body), not a graceful end of stream.
	const partial = `{"model":"llama3","response":"partial tok","done":false}`

	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&flakyHits, 1)
		w.Header().Set("Content-Length", strconv.Itoa(len(partial)*2))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, partial)
	}))
	defer flaky.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodHits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true}`))
	}))
	defer good.Close()

	r := router.New(
		config.RoutingConfig{
			Strategy:          "warm-first",
			Fallback:          "least-connections",
			PollIntervalMs:    2000,
			UpstreamTimeoutMs: 5000,
			MaxRetries:        2,
		},
		[]config.NodeConfig{
			{Name: "flaky", URL: flaky.URL, Runtime: "ollama"},
			{Name: "good", URL: good.URL, Runtime: "ollama"},
		},
		nil,
	)
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	marborSrv := httptest.NewServer(h)
	defer marborSrv.Close()

	resp, err := marborSrv.Client().Post(marborSrv.URL+"/api/generate", "application/json",
		newJSONBody(t, map[string]string{"model": "llama3"}))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // a read error here is expected - the stream was cut short

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200 (the header the aborted node already wrote)", resp.StatusCode)
	}
	if string(body) != partial {
		t.Errorf("client body = %q, want exactly the flaky node's partial write %q - no splicing of a second node's response onto this one",
			body, partial)
	}
	if got := atomic.LoadInt32(&goodHits); got != 0 {
		t.Errorf("good node received %d requests, want 0 - a retry fired after the first response byte", got)
	}
	if got := atomic.LoadInt32(&flakyHits); got != 1 {
		t.Errorf("flaky node received %d requests, want exactly 1", got)
	}

	flakyNode := r.FindNode("flaky")
	if flakyNode == nil {
		t.Fatal("flaky node not found in router")
	}
	flakyNode.Lock()
	hist := append([]bool(nil), flakyNode.SuccessHistory...)
	flakyNode.Unlock()
	if len(hist) != 1 || hist[0] != false {
		t.Errorf("flaky node SuccessHistory = %v, want [false] - a 200 status was written but the stream aborted mid-body, so the outcome must record failure, not a fabricated success",
			hist)
	}
}
