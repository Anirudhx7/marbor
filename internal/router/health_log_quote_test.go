package router

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// awkwardNodeName holds a space, a carriage return, a newline and an ESC
// byte, the characters a bare %s would write straight into the log.
const awkwardNodeName = "gpu 0\r\nforged line \x1b[31m"

// logTimestampPrefix is the standard log package prefix every real log line
// starts with. A node name that splits a line leaves a fragment without it.
var logTimestampPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`)

func newAwkwardRouter(t *testing.T, url string, runtimeName string) (*Router, *NodeState) {
	t.Helper()
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, Fallback: "least-connections"},
		[]config.NodeConfig{{Name: awkwardNodeName, URL: url, Runtime: runtimeName}}, nil)
	t.Cleanup(r.client.CloseIdleConnections)
	return r, r.nodes[0]
}

func assertQuotedNameLine(t *testing.T, out, event string) {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		t.Fatalf("no log output captured for %q", event)
	}
	var matching []string
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if !logTimestampPrefix.MatchString(line) {
			t.Errorf("log line split by the node name (no timestamp prefix): %q in %q", line, out)
		}
		if strings.Contains(line, event) {
			matching = append(matching, line)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("want exactly one log line containing %q, got %d\n%q", event, len(matching), out)
	}
	if !strings.Contains(matching[0], strconv.Quote(awkwardNodeName)) {
		t.Errorf("node name not %%q-escaped in %q", matching[0])
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("raw ESC reached the log: %q", out)
	}
}

func TestPollNodeLogsQuoteNodeName(t *testing.T) {
	t.Run("recovered probe panic", func(t *testing.T) {
		logBuf := captureLog(t)
		r, n := newAwkwardRouter(t, "http://127.0.0.1:1", "ollama")
		n.mu.Lock()
		n.probe = panicProbe{}
		n.mu.Unlock()

		r.pollNode(n)

		assertQuotedNameLine(t, logBuf.String(), "recovered panic in pollNode")
		n.mu.RLock()
		failures := n.Failures
		n.mu.RUnlock()
		if failures != 1 {
			t.Errorf("Failures = %d, want 1: markFailure must run after the recovered panic", failures)
		}
	})

	t.Run("auto-detect resolution", func(t *testing.T) {
		logBuf := captureLog(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/api/ps" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"models":[]}`))
				return
			}
			http.NotFound(w, req)
		}))
		t.Cleanup(srv.Close)
		r, n := newAwkwardRouter(t, srv.URL, "auto")

		r.pollNode(n)

		out := logBuf.String()
		assertQuotedNameLine(t, out, "auto-detect: node")
		if !strings.Contains(out, `resolved to runtime "ollama"`) {
			t.Errorf("auto-detect did not log the quoted runtime resolution: %q", out)
		}
		n.mu.RLock()
		stillAuto := n.autoDetect
		n.mu.RUnlock()
		if stillAuto {
			t.Error("autoDetect still true: the detection path under test did not run")
		}
	})
}
