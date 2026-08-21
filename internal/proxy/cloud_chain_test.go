package proxy

// cloud_chain_test.go -- tests for the priority-ordered cloud retry chain:
// when the local node is unavailable and more than one cloud provider is
// enabled, a failure on the highest-priority provider (before any response
// bytes are written to the client) must fall through to the next provider
// in priority order, rather than terminating the request.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

// newCloudChainHandler builds a Handler whose single local node is down and
// whose cloud chain has two enabled providers, so a request must fall
// through the chain to reach the client. The first provider is
// highest-priority so CloudChain (which is priority-sorted by router.New)
// attempts it first. It returns the admin.Server too so tests can inspect
// request-log/accounting side effects.
func newCloudChainHandler(t *testing.T, firstURL, secondURL string) (*Handler, *admin.Server) {
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
	return NewHandler(r, a, nil), a
}

// liveRequestEntries fetches /admin/requests/live and decodes the node +
// token fields, so tests can assert exactly how many accounting rows a
// fallback chain produced and which provider they were attributed to.
func liveRequestEntries(t *testing.T, a *admin.Server) []struct {
	Node   string `json:"routedTo"`
	Tokens int64  `json:"tokens"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	req.AddCookie(&http.Cookie{Name: "mesh_session", Value: a.AdminToken()})
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live requests status = %d", rec.Code)
	}
	var entries []struct {
		Node   string `json:"routedTo"`
		Tokens int64  `json:"tokens"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode live requests: %v", err)
	}
	return entries
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

	h, _ := newCloudChainHandler(t, badURL, good.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (chain should have fallen through to the good provider); body=%s", rec.Code, rec.Body.String())
	}
}

// TestCloudChainAccountsExactlyOnceOnFallback is a regression test for the
// double-accounting bug: when provider 1 fails (connection refused, before
// any bytes are written) and provider 2 serves the request, the parent
// proxyToCloud stack frame for provider 1 used to resume after
// serveAndRecoverAbort and run its own terminal accounting block a second
// time (metrics/logging/audit/cost-tracking), duplicating a request-log row
// and double-charging cloud cost for a response provider 1 never sent. The
// fix short-circuits the parent frame with a `delegated` flag once it hands
// off to the next provider in ErrorHandler. This test asserts exactly one
// request-log entry is produced for a 2-provider fallback-then-succeed
// chain, attributed to the provider that actually served the response.
func TestCloudChainAccountsExactlyOnceOnFallback(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badURL := bad.URL
	bad.Close() // closed before use: connection refused on every attempt

	h, a := newCloudChainHandler(t, badURL, good.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	entries := liveRequestEntries(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request-log entries, want exactly 1 (parent frame must not double-account after delegating to the next provider); entries=%v", len(entries), entries)
	}
	if entries[0].Node != "cloud:good" {
		t.Errorf("logged node = %q, want %q (the provider that actually served the response, not the one that failed)", entries[0].Node, "cloud:good")
	}
}

// TestCloudChainFallbackPreservesOllamaTranslation is a regression test for
// the shadowed-r bug: proxy.ErrorHandler's second parameter is the
// Director-mutated outbound request (path already rewritten to the cloud
// provider's translated path, e.g. /v1/chat/completions), not the original
// client request. The old code recursed into the next provider using that
// shadowed parameter, so isOllamaPath() on the fallback provider's call
// checked the already-translated path instead of the client's real
// /api/chat path and skipped wrapping the transport in
// translatingTransport - the client got raw OpenAI JSON instead of Ollama
// NDJSON. This test drives an /api/chat request through a 2-provider
// fallback chain (first fails, second succeeds via SSE) and asserts the
// response comes back as Ollama NDJSON.
func TestCloudChainFallbackPreservesOllamaTranslation(t *testing.T) {
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"Hel"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"lo"}}]}`,
	}
	good := sseCloud(t, chunks, 10, 20)
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	badURL := bad.URL
	bad.Close() // closed before use: connection refused on every attempt

	h, _ := newCloudChainHandler(t, badURL, good.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(`{"model":"llama3","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	if strings.Contains(respBody, "data: ") || strings.Contains(respBody, "[DONE]") {
		t.Fatalf("client received raw OpenAI SSE instead of translated Ollama NDJSON after fallback: %q", respBody)
	}

	lines := parseNDJSON(t, respBody)
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want >= 2 (content lines + final done:true)", len(lines))
	}
	last := lines[len(lines)-1]
	if last["done"] != true {
		t.Errorf("last line done = %v, want true", last["done"])
	}
	for i, l := range lines {
		msg, ok := l["message"].(map[string]interface{})
		if !ok {
			continue
		}
		if msg["role"] != "assistant" {
			t.Errorf("line %d: role = %v, want assistant", i, msg["role"])
		}
		if l["model"] != "llama3" {
			t.Errorf("line %d: model = %v, want llama3 (original client model)", i, l["model"])
		}
	}
}
