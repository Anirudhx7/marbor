package router

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// A panic while the node lock is held during telemetry apply must release the
// lock, or the recover path (which locks the same node) deadlocks.
func TestPollAgentHostPanicInApplyDoesNotDeadlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(marboragent.Telemetry{})
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:11434"},
	}, nil)
	n := r.nodes[0]
	r.applyTelemetryHook = func() { panic("boom in apply") }
	cfg := MarborAgentConfig{Enabled: true, Port: port, Scheme: "http"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.pollAgentHost(host, cfg, []*NodeState{n})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollAgentHost deadlocked after a panic inside the locked telemetry apply")
	}
	n.mu.RLock()
	failures := n.AgentFailures
	n.mu.RUnlock()
	if failures < 1 {
		t.Errorf("AgentFailures = %d, want the panic counted as unreachable", failures)
	}
}

func TestPollAgentHostPanicClearsTLSMismatchAndRateLimitsLog(t *testing.T) {
	logs := captureLog(t)
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:11434"},
	}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.AgentTLSMismatch = true
	n.mu.Unlock()
	r.client = nil
	cfg := MarborAgentConfig{Enabled: true, Port: 9200, Scheme: "http"}
	for i := 0; i < 3; i++ {
		r.pollAgentHost("h", cfg, []*NodeState{n})
	}
	n.mu.RLock()
	mm := n.AgentTLSMismatch
	n.mu.RUnlock()
	if mm {
		t.Error("AgentTLSMismatch still set after a recovered panic")
	}
	if c := strings.Count(logs.String(), "recovered panic polling agent"); c != 1 {
		t.Errorf("panic log lines = %d, want 1 (rate limited)", c)
	}
}

func TestTruncateForLog(t *testing.T) {
	long := strings.Repeat("x", 1000)
	if got := truncateForLog(long, 256); len(got) > 256+len("...") {
		t.Errorf("len = %d, want truncated", len(got))
	}
	if got := truncateForLog("short", 256); got != "short" {
		t.Errorf("got %q", got)
	}
}

func TestPortOfKeepsZeroForPortlessAndOrDefaultAppliesScheme(t *testing.T) {
	cases := []struct {
		in           string
		plain, deflt int
	}{
		{"http://host:8080", 8080, 8080},
		{"http://host", 0, 80},
		{"https://host", 0, 443},
		{"ftp://host", 0, 0},
		{"http://host:abc", 0, 0},
	}
	for _, c := range cases {
		if got := portOf(c.in); got != c.plain {
			t.Errorf("portOf(%q) = %d, want %d", c.in, got, c.plain)
		}
		if got := portOfOrDefault(c.in); got != c.deflt {
			t.Errorf("portOfOrDefault(%q) = %d, want %d", c.in, got, c.deflt)
		}
	}
}
