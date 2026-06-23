package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// mgmtTestHandler builds a Handler backed by a single healthy upstream that
// increments hits on every request it actually receives. The returned counter
// lets a test assert whether a request was forwarded upstream (hit) or blocked
// by the guard before forwarding (no hit).
func mgmtTestHandler(t *testing.T, allowManagement bool) (*Handler, *int64) {
	t.Helper()
	var hits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true}`))
	}))
	t.Cleanup(upstream.Close)

	r := router.New(
		config.RoutingConfig{
			Strategy:          "warm-first",
			Fallback:          "least-connections",
			PollIntervalMs:    2000,
			UpstreamTimeoutMs: 5000,
			MaxRetries:        2,
		},
		[]config.NodeConfig{{Name: "good", URL: upstream.URL, Runtime: "ollama"}},
		nil,
	)
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	h.SetAllowManagementEndpoints(allowManagement)
	return h, &hits
}

// TestManagementGuardBlocksByDefault verifies that with the guard on (default),
// destructive management paths return 403 and are NOT forwarded upstream.
func TestManagementGuardBlocksByDefault(t *testing.T) {
	for _, path := range []string{"/api/delete", "/api/pull", "/api/push", "/api/create", "/api/copy", "/api/blobs", "/api/blobs/sha256:abc"} {
		h, hits := mgmtTestHandler(t, false)
		req := httptest.NewRequest(http.MethodPost, path,
			newJSONBody(t, map[string]string{"model": "llama3"}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not permitted") {
			t.Errorf("%s: body = %q, want it to mention 'not permitted'", path, rec.Body.String())
		}
		if n := atomic.LoadInt64(hits); n != 0 {
			t.Errorf("%s: upstream was hit %d times, want 0 (must not forward)", path, n)
		}
	}
}

// TestManagementGuardAllowsInference verifies inference paths are never blocked,
// in both guard modes.
func TestManagementGuardAllowsInference(t *testing.T) {
	for _, allow := range []bool{false, true} {
		h, hits := mgmtTestHandler(t, allow)
		req := httptest.NewRequest(http.MethodPost, "/api/generate",
			newJSONBody(t, map[string]string{"model": "llama3"}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("allow=%v: /api/generate status = %d, want 200 (body: %s)", allow, rec.Code, rec.Body.String())
		}
		if n := atomic.LoadInt64(hits); n != 1 {
			t.Errorf("allow=%v: /api/generate forwarded %d times, want 1", allow, n)
		}
	}
}

// TestManagementGuardEscapeHatch verifies that with allow_management_endpoints
// true, a management path is forwarded upstream (reaches the stub).
func TestManagementGuardEscapeHatch(t *testing.T) {
	h, hits := mgmtTestHandler(t, true)
	req := httptest.NewRequest(http.MethodPost, "/api/delete",
		newJSONBody(t, map[string]string{"model": "llama3"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("/api/delete with escape hatch: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Errorf("/api/delete with escape hatch: forwarded %d times, want 1", n)
	}
}

// TestIsBlockedManagementPath unit-tests the path classifier directly.
func TestIsBlockedManagementPath(t *testing.T) {
	blocked := []string{"/api/delete", "/api/pull", "/api/push", "/api/create", "/api/copy", "/api/blobs", "/api/blobs/sha256:deadbeef"}
	for _, p := range blocked {
		if !isBlockedManagementPath(p) {
			t.Errorf("isBlockedManagementPath(%q) = false, want true", p)
		}
	}
	allowed := []string{"/api/generate", "/api/chat", "/api/embed", "/api/embeddings", "/api/tags", "/api/ps", "/api/show", "/api/version", "/v1/models", "/v1/chat/completions"}
	for _, p := range allowed {
		if isBlockedManagementPath(p) {
			t.Errorf("isBlockedManagementPath(%q) = true, want false", p)
		}
	}
}

// TestCloudTransportHasHeaderTimeout verifies Fix B: the cloud transport is
// built with a non-zero ResponseHeaderTimeout derived from the router's
// upstream timeout, so a hung cloud provider cannot leak connections. No
// overall client Timeout is set (that would break R2 streaming).
func TestCloudTransportHasHeaderTimeout(t *testing.T) {
	const timeoutMs = 7000
	r := router.New(
		config.RoutingConfig{
			Strategy:          "warm-first",
			PollIntervalMs:    2000,
			UpstreamTimeoutMs: timeoutMs,
		},
		[]config.NodeConfig{{Name: "node1", URL: "http://localhost:11434"}},
		nil,
	)
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	tr := h.cloudRoundTripper()
	if tr.ResponseHeaderTimeout != timeoutMs*time.Millisecond {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, timeoutMs*time.Millisecond)
	}
	// Returned transport must be the same shared instance on repeat calls.
	if h.cloudRoundTripper() != tr {
		t.Error("cloudRoundTripper returned a different instance on second call; want shared")
	}
}
