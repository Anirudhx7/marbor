package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
)

// awkwardNodeName holds a space, a newline and an ESC byte, the characters a
// bare %s would write straight into the log.
const awkwardNodeName = "gpu 0\nforged line \x1b[31m"

type quotePanicProbe struct{}

func (quotePanicProbe) Probe(context.Context, string) (runtimepkg.ProbeResult, error) {
	panic("boom")
}

func logLinesContaining(out, event string) []string {
	var hits []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, event) {
			hits = append(hits, line)
		}
	}
	return hits
}

func assertQuotedNameLine(t *testing.T, out, event string) {
	t.Helper()
	hits := logLinesContaining(out, event)
	if len(hits) != 1 {
		t.Fatalf("want exactly one log line containing %q, got %d\n%q", event, len(hits), out)
	}
	if !strings.Contains(hits[0], strconv.Quote(awkwardNodeName)) {
		t.Errorf("node name not %%q-escaped in %q", hits[0])
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("raw ESC reached the log: %q", out)
	}
	if strings.Contains(out, "forged line") && len(logLinesContaining(out, "forged line")) != 1 {
		t.Errorf("name split the log into extra lines: %q", out)
	}
}

func newAwkwardRouter(t *testing.T, url string) (*Router, *NodeState) {
	t.Helper()
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, Fallback: "least-connections"},
		[]config.NodeConfig{{Name: "placeholder", URL: url}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Name = awkwardNodeName
	n.mu.Unlock()
	return r, n
}

func TestPollNodeRecoverLogQuotesNodeName(t *testing.T) {
	logBuf := captureLog(t)
	r, n := newAwkwardRouter(t, "http://127.0.0.1:1")
	n.mu.Lock()
	n.autoDetect = false
	n.probe = quotePanicProbe{}
	n.mu.Unlock()

	r.pollNode(n)

	assertQuotedNameLine(t, logBuf.String(), "recovered panic in pollNode")
}

func TestAutoDetectLogQuotesNodeName(t *testing.T) {
	logBuf := captureLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/ps" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		http.NotFound(w, req)
	}))
	defer srv.Close()
	r, n := newAwkwardRouter(t, srv.URL)
	n.mu.Lock()
	n.Runtime = "auto"
	n.autoDetect = true
	n.mu.Unlock()

	r.pollNode(n)

	assertQuotedNameLine(t, logBuf.String(), "auto-detect: node")
}
