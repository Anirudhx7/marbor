package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// liveRequestEntry mirrors the fields of admin.RequestLog the proxy tests
// care about.
type liveRequestEntry struct {
	Model  string `json:"model"`
	Node   string `json:"node"`
	Status string `json:"status"`
}

// fetchLiveRequests reads the admin request log through the real
// /admin/requests/live endpoint (default token "admin" for a zero config).
func fetchLiveRequests(t *testing.T, a *admin.Server) []liveRequestEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	req.Header.Set("Authorization", "Bearer "+a.AdminToken())
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live requests status = %d, want 200", rec.Code)
	}
	var entries []liveRequestEntry
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode live requests: %v", err)
	}
	return entries
}

func TestProxyNoHealthyNodes(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "test", URL: "http://localhost:1", GPUModel: "V100"},
	}, nil)
	// Mark nodes as unhealthy
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.2:8b"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestTranslateCloudPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/api/chat", "/v1/chat/completions"},
		{"/api/generate", "/v1/completions"},
		{"/api/embeddings", "/v1/embeddings"},
		{"/api/tags", "/api/tags"},
		{"/unknown/path", "/unknown/path"},
	}
	for _, tc := range cases {
		got := translateCloudPath(tc.in)
		if got != tc.want {
			t.Errorf("translateCloudPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRewriteModelField(t *testing.T) {
	body := []byte(`{"model":"old-model","prompt":"hello"}`)
	out := rewriteModelField(body, "gpt-4o")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("rewriteModelField output invalid JSON: %v", err)
	}
	var model string
	if err := json.Unmarshal(m["model"], &model); err != nil {
		t.Fatalf("model field not a string: %v", err)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}

	// bad JSON returns original
	bad := []byte(`not-json`)
	got := rewriteModelField(bad, "gpt-4o")
	if !bytes.Equal(got, bad) {
		t.Error("expected original bytes returned on bad JSON")
	}
}

func TestProxyFallsBackToCloud(t *testing.T) {
	// Fake cloud backend that records the Authorization header it received
	var gotAuthHeader string
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[]}`))
	}))
	defer cloudSrv.Close()

	clouds := []config.CloudProvider{
		{
			Name:            "fake-openai",
			Provider:        "openai",
			BaseURL:         cloudSrv.URL,
			APIKey:          "test-key",
			DefaultModel:    "gpt-4o",
			CostPer1KTokens: 0.002,
			Enabled:         true,
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
	h := NewHandler(r, a, nil)
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{"model":"llama3.2:8b","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == 503 {
		t.Error("expected cloud fallback, got 503 (no healthy nodes)")
	}
	if gotAuthHeader != "Bearer test-key" {
		t.Errorf("cloud received Authorization = %q, want %q", gotAuthHeader, "Bearer test-key")
	}
}

// TestProxyCloudBudgetExceededBlocksFallback verifies that once cumulative
// cloud spend has reached the configured daily cap, a request that would
// otherwise overflow to cloud gets a clean 503 instead of reaching the
// provider - the mesh must never keep spending past an operator-set cap.
func TestProxyCloudBudgetExceededBlocksFallback(t *testing.T) {
	hit := false
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[]}`))
	}))
	defer cloudSrv.Close()

	tmpDB := filepath.Join(t.TempDir(), "cloud-budget-proxy.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	clouds := []config.CloudProvider{
		{
			Name:            "fake-openai",
			Provider:        "openai",
			BaseURL:         cloudSrv.URL,
			APIKey:          "test-key",
			DefaultModel:    "gpt-4o",
			CostPer1KTokens: 2.0,
			Enabled:         true,
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

	a := admin.NewServer(r, nil, config.Config{
		CloudBudget: config.CloudBudgetConfig{DailyUSDCap: 1.0},
	}, st)
	// $2.00 spent already, over the $1.00 daily cap.
	a.TrackCloudCostModel("gpt-4o", 2.0, 1000)

	h := NewHandler(r, a, nil)
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{"model":"llama3.2:8b","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when cloud budget is exceeded; body: %s", rec.Code, rec.Body.String())
	}
	if hit {
		t.Error("request reached the cloud provider; cloud budget cap must block dispatch before it is called")
	}
}

// TestAnthropicCompletionsReturns501 verifies that a /v1/completions request
// routed to an Anthropic overflow provider is rejected with a clean 501 by the
// mesh, rather than being proxied to Anthropic (which has no such endpoint) and
// returning a confusing raw error to the client.
func TestAnthropicCompletionsReturns501(t *testing.T) {
	hit := false
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer cloudSrv.Close()

	clouds := []config.CloudProvider{
		{Name: "claude", Provider: "anthropic", BaseURL: cloudSrv.URL, APIKey: "sk-ant", DefaultModel: "claude-3-5-sonnet", Enabled: true},
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
	h := NewHandler(r, a, nil)
	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader([]byte(`{"model":"x","prompt":"hi"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("Anthropic /v1/completions: got %d, want 501; body: %s", rec.Code, rec.Body.String())
	}
	if hit {
		t.Error("request reached the Anthropic backend; it must be rejected before proxying")
	}
}

func TestProxyNoFallbackWhenCloudDisabled(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-x", Enabled: false},
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
	h := NewHandler(r, a, nil)
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.2:8b"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 when cloud disabled and no healthy nodes", rec.Code)
	}
}

// newCloudFallbackHandler builds a Handler whose only node is down so every
// request overflows to the given cloud provider, with a real audit logger.
func newCloudFallbackHandler(t *testing.T, cloud config.CloudProvider) (*Handler, *admin.Server, *audit.Logger) {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, []config.CloudProvider{cloud})
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	tmpDB := filepath.Join(t.TempDir(), "audit.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	al := audit.New(st, true)
	t.Cleanup(func() { al.Close() })
	return NewHandler(r, a, al), a, al
}

func TestCloudModelMappingVisibleInLogs(t *testing.T) {
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[],"usage":{"total_tokens":42}}`))
	}))
	defer cloudSrv.Close()

	h, a, al := newCloudFallbackHandler(t, config.CloudProvider{
		Name:            "fake-openai",
		Provider:        "openai",
		BaseURL:         cloudSrv.URL,
		APIKey:          "test-key",
		DefaultModel:    "gpt-4o-mini",
		CostPer1KTokens: 0.002,
		Enabled:         true,
	})

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Request log shows the rewrite as "<original> -> <cloud model>".
	entries := fetchLiveRequests(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request log entries, want 1", len(entries))
	}
	if want := "llama3 -> gpt-4o-mini"; entries[0].Model != want {
		t.Errorf("request log model = %q, want %q", entries[0].Model, want)
	}

	// Audit entry keeps the original model and records the cloud model.
	audits, err := al.Query(audit.QueryOptions{})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(audits))
	}
	if audits[0].Model != "llama3" {
		t.Errorf("audit model = %q, want llama3", audits[0].Model)
	}
	if audits[0].CloudModel != "gpt-4o-mini" {
		t.Errorf("audit cloud_model = %q, want gpt-4o-mini", audits[0].CloudModel)
	}
	if !audits[0].Cloud {
		t.Error("audit entry should be marked cloud")
	}
}

func TestCloudModelNotRewrittenLogsPlainModel(t *testing.T) {
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[]}`))
	}))
	defer cloudSrv.Close()

	h, a, al := newCloudFallbackHandler(t, config.CloudProvider{
		Name:     "fake-openai",
		Provider: "openai",
		BaseURL:  cloudSrv.URL,
		APIKey:   "test-key",
		// DefaultModel empty: no rewrite happens.
		Enabled: true,
	})

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entries := fetchLiveRequests(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request log entries, want 1", len(entries))
	}
	if entries[0].Model != "llama3" {
		t.Errorf("request log model = %q, want plain llama3 when no rewrite", entries[0].Model)
	}

	audits, err := al.Query(audit.QueryOptions{})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(audits))
	}
	if audits[0].CloudModel != "" {
		t.Errorf("audit cloud_model = %q, want empty when no rewrite", audits[0].CloudModel)
	}
}

// TestProxy_AuthHeaderStripped verifies that the Authorization header sent by
// the client is never forwarded to the upstream Ollama node.
func TestProxy_AuthHeaderStripped(t *testing.T) {
	var gotAuthHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	r.Nodes()[0].Lock()
	r.Nodes()[0].Healthy = true
	r.Nodes()[0].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3"}`)))
	req.Header.Set("Authorization", "Bearer client-secret-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotAuthHeader != "" {
		t.Errorf("upstream received Authorization header %q; want it stripped", gotAuthHeader)
	}
}

// TestProxy_BodyCapEnforced verifies that a request body exceeding 32 MiB is
// rejected with 413 and never forwarded to upstream.
func TestProxy_BodyCapEnforced(t *testing.T) {
	var upstreamCalled int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&upstreamCalled, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	r.Nodes()[0].Lock()
	r.Nodes()[0].Healthy = true
	r.Nodes()[0].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	// Build a body 1 byte over the 32 MiB cap.
	oversized := make([]byte, 32*1024*1024+1)
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(oversized))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for oversized body", rec.Code)
	}
	if atomic.LoadInt32(&upstreamCalled) != 0 {
		t.Error("upstream was called despite oversized body; should have been rejected before forwarding")
	}
}

// TestProxy_ManagementEndpointsBlocked verifies that each Ollama management
// endpoint returns 403 Forbidden by default when proxied through the mesh.
func TestProxy_ManagementEndpointsBlocked(t *testing.T) {
	var upstreamCalled int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&upstreamCalled, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	r.Nodes()[0].Lock()
	r.Nodes()[0].Healthy = true
	r.Nodes()[0].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	// allowManagement defaults to false - management endpoints must be blocked.

	managementPaths := []string{
		"/api/delete",
		"/api/pull",
		"/api/push",
		"/api/copy",
		"/api/create",
		"/api/blobs",
		"/api/blobs/sha256:abc123",
	}

	for _, path := range managementPaths {
		t.Run(path, func(t *testing.T) {
			atomic.StoreInt32(&upstreamCalled, 0)
			req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(`{"model":"llama3"}`)))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("path %s: status = %d, want 403", path, rec.Code)
			}
			if atomic.LoadInt32(&upstreamCalled) != 0 {
				t.Errorf("path %s: upstream was called; management endpoint should be blocked before forwarding", path)
			}
		})
	}
}

// TestProxy_XRequestIDForwarded verifies that the X-Request-ID header is
// forwarded to the upstream Ollama node.
func TestProxy_XRequestIDForwarded(t *testing.T) {
	var gotRequestID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestID = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	r.Nodes()[0].Lock()
	r.Nodes()[0].Healthy = true
	r.Nodes()[0].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotRequestID == "" {
		t.Error("upstream did not receive X-Request-ID header; want non-empty")
	}
}

func TestProxyExtractAndRoute(t *testing.T) {
	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11435", GPUModel: "RTX 4090", Runtime: "ollama"},
	}, nil)
	r.Nodes()[0].Lock()
	r.Nodes()[0].Healthy = true
	r.Nodes()[0].Unlock()

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.2:8b"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Will fail to dial localhost, but should get past routing
	if rec.Code != 502 && rec.Code != 503 {
		t.Logf("got status %d (expected 502 bad gateway from dial failure)", rec.Code)
	}
}
