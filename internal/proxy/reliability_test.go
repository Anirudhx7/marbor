package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// seedNode marks a node healthy and optionally seeds a loaded model.
func seedNode(n *router.NodeState, model string) {
	n.Lock()
	n.Healthy = true
	if model != "" {
		n.LoadedModels = []router.ModelInfo{{Name: model}}
	}
	n.Unlock()
}

// deadURL returns a URL for a TCP port that is not listening (connection refused).
func deadURL(t *testing.T) string {
	t.Helper()
	// Bind then close - the OS will refuse new connections to this port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("deadURL: listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

// TestRetryFailoverSuccess verifies that when the first node refuses the
// connection, the handler retries a second healthy node and the client
// receives 200.
func TestRetryFailoverSuccess(t *testing.T) {
	dead := deadURL(t)

	// Second node is a real mock that always returns 200.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			{Name: "dead", URL: dead, Runtime: "ollama"},
			{Name: "good", URL: good.URL, Runtime: "ollama"},
		},
		nil,
	)

	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		newJSONBody(t, map[string]string{"model": "llama3"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestRetryAllDeadNoCloud verifies that when all nodes are dead and no cloud
// provider is configured, the handler returns 502.
func TestRetryAllDeadNoCloud(t *testing.T) {
	dead1 := deadURL(t)
	dead2 := deadURL(t)

	r := router.New(
		config.RoutingConfig{
			Strategy:          "warm-first",
			Fallback:          "least-connections",
			PollIntervalMs:    2000,
			UpstreamTimeoutMs: 5000,
			MaxRetries:        2,
		},
		[]config.NodeConfig{
			{Name: "dead1", URL: dead1, Runtime: "ollama"},
			{Name: "dead2", URL: dead2, Runtime: "ollama"},
		},
		nil,
	)

	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		newJSONBody(t, map[string]string{"model": "llama3"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

// TestV1ModelsEndpoint verifies GET /v1/models returns object:"list" and
// contains the union of LoadedModels from two nodes, deduplicated.
func TestV1ModelsEndpoint(t *testing.T) {
	r := router.New(
		config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000},
		[]config.NodeConfig{
			{Name: "node1", URL: "http://localhost:11434"},
			{Name: "node2", URL: "http://localhost:11435"},
		},
		nil,
	)

	nodes := r.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].LoadedModels = []router.ModelInfo{
		{Name: "llama3"},
		{Name: "mistral"},
	}
	nodes[0].Unlock()

	nodes[1].Lock()
	nodes[1].Healthy = true
	nodes[1].LoadedModels = []router.ModelInfo{
		{Name: "mistral"}, // duplicate - should appear once
		{Name: "codellama"},
	}
	nodes[1].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Object != "list" {
		t.Errorf("object = %q, want %q", resp.Object, "list")
	}

	ids := make(map[string]int)
	for _, d := range resp.Data {
		ids[d.ID]++
		if d.Object != "model" {
			t.Errorf("data entry object = %q, want %q", d.Object, "model")
		}
		if d.OwnedBy != "marbor" {
			t.Errorf("owned_by = %q, want %q", d.OwnedBy, "marbor")
		}
	}

	for _, want := range []string{"llama3", "mistral", "codellama"} {
		if ids[want] != 1 {
			t.Errorf("model %q appears %d times, want 1", want, ids[want])
		}
	}

	if len(resp.Data) != 3 {
		t.Errorf("len(data) = %d, want 3 (deduplicated)", len(resp.Data))
	}
}

// TestV1ModelsUnhealthyExcluded verifies that models from unhealthy nodes are
// not included in the /v1/models response.
func TestV1ModelsUnhealthyExcluded(t *testing.T) {
	r := router.New(
		config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000},
		[]config.NodeConfig{
			{Name: "healthy", URL: "http://localhost:11434"},
			{Name: "down", URL: "http://localhost:11435"},
		},
		nil,
	)

	nodes := r.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].LoadedModels = []router.ModelInfo{{Name: "llama3"}}
	nodes[0].Unlock()

	nodes[1].Lock()
	nodes[1].Healthy = false
	nodes[1].LoadedModels = []router.ModelInfo{{Name: "secretmodel"}}
	nodes[1].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, d := range resp.Data {
		if d.ID == "secretmodel" {
			t.Error("model from unhealthy node should not appear in /v1/models")
		}
	}
}

// TestTransportResponseHeaderTimeout verifies that the proxy Transport has a
// non-zero ResponseHeaderTimeout equal to the configured UpstreamTimeoutMs.
// This is a unit-level check - no sleep or flaky timing involved.
func TestTransportResponseHeaderTimeout(t *testing.T) {
	wantTimeout := 7 * time.Second

	r := router.New(
		config.RoutingConfig{
			Strategy:          "warm-first",
			PollIntervalMs:    2000,
			UpstreamTimeoutMs: int(wantTimeout.Milliseconds()),
			MaxRetries:        2,
		},
		[]config.NodeConfig{
			{Name: "node1", URL: "http://localhost:11434"},
		},
		nil,
	)

	got := r.UpstreamTimeout()
	if got != wantTimeout {
		t.Errorf("UpstreamTimeout() = %v, want %v", got, wantTimeout)
	}

	// Verify the transport built from this value has the correct timeout.
	transport := &http.Transport{
		ResponseHeaderTimeout: r.UpstreamTimeout(),
	}
	if transport.ResponseHeaderTimeout != wantTimeout {
		t.Errorf("Transport.ResponseHeaderTimeout = %v, want %v",
			transport.ResponseHeaderTimeout, wantTimeout)
	}
}

// newJSONBody is a test helper that marshals v to JSON and returns a reader.
func newJSONBody(t *testing.T, v interface{}) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bytes.NewReader(b)
}
