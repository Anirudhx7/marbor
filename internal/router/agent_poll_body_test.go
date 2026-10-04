package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

const validStatusJSON = `{"agent": {"version": "v1.0.0", "protocol_version": 1}}`

// paddedStatusBody returns a valid status object padded with trailing
// whitespace to exactly size bytes.
func paddedStatusBody(size int) []byte {
	return []byte(validStatusJSON + strings.Repeat(" ", size-len(validStatusJSON)))
}

// pollOneAgent polls a router with a single node against an agent served by h,
// n times, and returns the router.
func pollOneAgent(t *testing.T, h http.HandlerFunc, n int) *Router {
	t.Helper()
	psSrv := nodePSServer()
	t.Cleanup(psSrv.Close)
	agentSrv := httptest.NewServer(h)
	t.Cleanup(agentSrv.Close)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, agentSrv.URL), "tok", "http")
	for i := 0; i < n; i++ {
		r.pollAgentHosts()
	}
	return r
}

func agentState(r *Router) (present bool, failures int) {
	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	return r.nodes[0].AgentPresent, r.nodes[0].AgentFailures
}

func TestPollAgentStatusBodyCapBoundary(t *testing.T) {
	cases := []struct {
		name        string
		size        int
		wantPresent bool
	}{
		{"exactly the cap is accepted", maxAgentStatusBodyBytes, true},
		{"one byte over the cap is rejected", maxAgentStatusBodyBytes + 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logBuf := captureLog(t)
			r := pollOneAgent(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(paddedStatusBody(tc.size))
			}, 1)
			present, failures := agentState(r)
			if present != tc.wantPresent {
				t.Errorf("AgentPresent = %v, want %v", present, tc.wantPresent)
			}
			if !tc.wantPresent && failures != 1 {
				t.Errorf("AgentFailures = %d, want 1", failures)
			}
			oversizeLogged := strings.Contains(logBuf.String(), "status response exceeds")
			if tc.wantPresent && oversizeLogged {
				t.Errorf("size %d is within the cap but an oversize rejection was logged\n%s", tc.size, logBuf.String())
			}
			if !tc.wantPresent && !oversizeLogged {
				t.Errorf("size %d is over the cap but no oversize rejection was logged\n%s", tc.size, logBuf.String())
			}
		})
	}
}

func TestPollAgentStatusMalformedJSONIsUnreachable(t *testing.T) {
	r := pollOneAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent": {"version": `))
	}, 1)
	present, failures := agentState(r)
	if present || failures != 1 {
		t.Errorf("AgentPresent=%v AgentFailures=%d, want false/1 for malformed JSON", present, failures)
	}
}

// TestPollAgentStatusMidBodyDropIsLoggedAndUnreachable proves a connection that
// dies mid-body is reported as a read failure (rate-limited, with the host and
// error) rather than being silently folded into "bad JSON".
func TestPollAgentStatusMidBodyDropIsLoggedAndUnreachable(t *testing.T) {
	logBuf := captureLog(t)
	r := pollOneAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte(`{"agent": {"vers`))
	}, 2)

	present, failures := agentState(r)
	if present || failures != 2 {
		t.Errorf("AgentPresent=%v AgentFailures=%d, want false/2 for a dropped body", present, failures)
	}
	out := logBuf.String()
	if got := strings.Count(out, "reading agent status from host"); got != 1 {
		t.Errorf("read failure logged %d times across 2 polls, want 1 (rate-limited)\n%s", got, out)
	}
	const marker = "reading agent status from host"
	line := out[strings.Index(out, marker):]
	if i := strings.Index(line, "\n"); i >= 0 {
		line = line[:i]
	}
	host := r.nodes[0].Host
	if !strings.Contains(line, fmt.Sprintf("%q", host)) {
		t.Errorf("host %q not quoted in read failure log: %s", host, line)
	}
	const after, before = "failed: ", "; treating"
	start := strings.Index(line, after)
	end := strings.Index(line, before)
	if start < 0 || end < 0 || end <= start+len(after) {
		t.Errorf("read failure log carries no error text: %s", line)
	}
}

// TestAgentStatusCapConstantsPinned makes a change to the literal caps a
// deliberate test edit.
func TestAgentStatusCapConstantsPinned(t *testing.T) {
	if maxAgentStatusBodyBytes != 8<<20 {
		t.Errorf("maxAgentStatusBodyBytes = %d, want %d", maxAgentStatusBodyBytes, 8<<20)
	}
	if maxHostEvidenceAddrs != 64 {
		t.Errorf("maxHostEvidenceAddrs = %d, want 64", maxHostEvidenceAddrs)
	}
	if maxHostEvidenceDeployments != 64 {
		t.Errorf("maxHostEvidenceDeployments = %d, want 64", maxHostEvidenceDeployments)
	}
}
