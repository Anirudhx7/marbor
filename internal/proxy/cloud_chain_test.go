package proxy

// cloud_chain_test.go -- tests for the priority-ordered cloud retry chain:
// when the local node is unavailable and more than one cloud provider is
// enabled, a failure on the highest-priority provider (before any response
// bytes are written to the client) must fall through to the next provider
// in priority order, rather than terminating the request.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// newCloudChainHandler builds a Handler whose single local node is down and
// whose cloud chain has two enabled providers, so a request must fall
// through the chain to reach the client. The first provider is
// highest-priority so CloudChain (which is priority-sorted by router.New)
// attempts it first.
func newCloudChainHandler(t *testing.T, firstURL, secondURL string) *Handler {
	t.Helper()
	clouds := []config.CloudProvider{
		{
			Name:     "bad",
			Provider: "openai",
			BaseURL:  firstURL,
			APIKey:   "sk-a",
			Priority: 10,
			Enabled:  true,
		},
		{
			Name:     "good",
			Provider: "openai",
			BaseURL:  secondURL,
			APIKey:   "sk-b",
			Priority: 5,
			Enabled:  true,
		},
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, clouds)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	return NewHandler(r, a, nil)
}

// TestCloudChainFallsThroughOnConnectionFailure verifies that when the
// highest-priority cloud provider is unreachable (connection refused before
// any bytes are written), the request falls through to the next provider in
// the chain instead of terminating with a 502.
func TestCloudChainFallsThroughOnConnectionFailure(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badURL := bad.URL
	bad.Close() // closed before use: connection refused on every attempt

	h := newCloudChainHandler(t, badURL, good.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (chain should have fallen through to the good provider); body=%s", rec.Code, rec.Body.String())
	}
}
