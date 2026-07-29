package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func newHealthCheckRequest(t *testing.T, s *Server, node string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/"+node+"/health-check", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", node)
	return req
}

// TestHandleNodeHealthCheck_FallsBackToDirectProbeForAgentlessNode verifies
// a node with no Node Agent configured gets a real probe result (200, not
// the old hard 501) and that the on-demand probe never mutates NodeState -
// Healthy/LastPollAt stay exactly what the periodic poller last set them
// to, since a one-off admin click must not reset the poller's own
// failure-count/hysteresis state outside its normal cadence.
func TestHandleNodeHealthCheck_FallsBackToDirectProbeForAgentlessNode(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	nodes := s.router.Nodes()
	var before struct {
		healthy    bool
		lastPollAt int64
	}
	for _, n := range nodes {
		if n.Name != "gpu-0" {
			continue
		}
		n.RLock()
		before.healthy = n.Healthy
		before.lastPollAt = n.LastPollAt.UnixNano()
		n.RUnlock()
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newHealthCheckRequest(t, s, "gpu-0"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (real fallback probe, not 501), got %d: %s", res.StatusCode, w.Body.String())
	}

	var result nodeHealthCheckResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Nothing listens on localhost:11434 in this test, so the probe must
	// report a real failure - never a fabricated success (R1).
	if result.OK {
		t.Errorf("expected OK=false (nothing listening on the probed port), got OK=true")
	}
	if result.Error == "" {
		t.Errorf("expected a real error message from the failed probe, got empty string")
	}

	for _, n := range nodes {
		if n.Name != "gpu-0" {
			continue
		}
		n.RLock()
		afterHealthy := n.Healthy
		afterLastPollAt := n.LastPollAt.UnixNano()
		n.RUnlock()
		if afterHealthy != before.healthy {
			t.Errorf("on-demand probe mutated Healthy: before=%v after=%v", before.healthy, afterHealthy)
		}
		if afterLastPollAt != before.lastPollAt {
			t.Errorf("on-demand probe mutated LastPollAt: before=%v after=%v", before.lastPollAt, afterLastPollAt)
		}
	}
}

// TestHandleNodeHealthCheck_NodeNotFound mirrors the other handlers' 404
// behavior for a name that matches no current node.
func TestHandleNodeHealthCheck_NodeNotFound(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newHealthCheckRequest(t, s, "does-not-exist"))

	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Result().StatusCode)
	}
}
