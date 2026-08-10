package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
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
	req.AddCookie(&http.Cookie{Name: "mesh_session", Value: a.AdminToken()})
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

// waitForAuditEntries polls al.Query until it returns at least want entries
// or the timeout elapses. audit.Logger.Log enqueues onto a channel drained
// by a background writer goroutine (internal/audit/audit.go), so a query
// issued immediately after ServeHTTP returns can race the writer under load
// - this makes that race deterministic in tests instead of relying on the
// writer winning a scheduling race every time.
func waitForAuditEntries(t *testing.T, al *audit.Logger, want int) []audit.Entry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var audits []audit.Entry
	for {
		var err error
		audits, err = al.Query(audit.QueryOptions{})
		if err != nil {
			t.Fatalf("audit query: %v", err)
		}
		if len(audits) >= want || time.Now().After(deadline) {
			return audits
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	a.TrackCloudCostModel("testkey", "openai", "gpt-4o", 2.0, 1000)

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

// TestProxyContextLengthGuardRejectsOversizedRequest verifies that a request
// whose estimated token count (char-count/4 heuristic) exceeds the model's
// operator-declared context window is rejected with 400 before it ever
// reaches a backend node.
func TestProxyContextLengthGuardRejectsOversizedRequest(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3.2:8b","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)

	a := admin.NewServer(r, nil, config.Config{
		ContextWindows: map[string]int{"llama3.2:8b": 8},
	})
	h := NewHandler(r, a, nil)

	// 100 chars / 4 = 25 estimated tokens, well over the 8-token test window.
	longPrompt := strings.Repeat("x", 100)
	body := []byte(fmt.Sprintf(`{"model":"llama3.2:8b","prompt":%q}`, longPrompt))
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when estimated tokens exceed the context window; body: %s", rec.Code, rec.Body.String())
	}
	if hit {
		t.Error("request reached the backend node; the context-length guard must reject before routing")
	}
}

// TestProxyContextLengthGuardAllowsUndeclaredModel verifies that a model with
// no configured context window is never blocked - the guard fails open.
func TestProxyContextLengthGuardAllowsUndeclaredModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3.2:8b","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	longPrompt := strings.Repeat("x", 100)
	body := []byte(fmt.Sprintf(`{"model":"llama3.2:8b","prompt":%q}`, longPrompt))
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when the model has no declared context window; body: %s", rec.Code, rec.Body.String())
	}
}

// TestProxyQuantizationFallback_SubstitutesWhenPrimaryDoesNotFit verifies the
// end-to-end opt-in fallback path: when the requested model provably does
// not fit anywhere and a declared, already-downloaded, fitting alternate
// exists, the request is served with the alternate, the substitution is
// surfaced via a response header, and the actual model reaches the backend.
func TestProxyQuantizationFallback_SubstitutesWhenPrimaryDoesNotFit(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[
				{"name":"llama3.1:70b","size":41943040000},
				{"name":"llama3.1:70b-q4_K_M","size":4194304000}
			]}`))
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3.1:70b-q4_K_M","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{
		Strategy:       "warm-first",
		FallbackChains: map[string][]string{"llama3.1:70b": {"llama3.1:70b-q4_K_M"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama", VRAMTotalMB: 8192},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.VRAMTotalMB = 8192
		n.VRAMUsedMB = 0
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.1:70b","prompt":"hi"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if want := "llama3.1:70b -> llama3.1:70b-q4_K_M"; rec.Header().Get("X-Ollama-Mesh-Model-Fallback") != want {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want %q", rec.Header().Get("X-Ollama-Mesh-Model-Fallback"), want)
	}
	if gotModel != "llama3.1:70b-q4_K_M" {
		t.Errorf("backend received model = %q, want llama3.1:70b-q4_K_M", gotModel)
	}
}

// TestProxyQuantizationFallback_NoSubstitutionWhenPrimaryFits verifies that
// the fallback chain is never consulted when the primary model already fits
// - no header, no substitution, even though a chain is configured.
func TestProxyQuantizationFallback_NoSubstitutionWhenPrimaryFits(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"llama3.1:70b","size":41943040000}]}`))
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3.1:70b","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{
		Strategy:       "warm-first",
		FallbackChains: map[string][]string{"llama3.1:70b": {"llama3.1:70b-q4_K_M"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama", VRAMTotalMB: 65536},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.VRAMTotalMB = 65536
		n.VRAMUsedMB = 0
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"llama3.1:70b","prompt":"hi"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Ollama-Mesh-Model-Fallback"); got != "" {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want empty (primary model fits)", got)
	}
	if gotModel != "llama3.1:70b" {
		t.Errorf("backend received model = %q, want llama3.1:70b (no substitution)", gotModel)
	}
}

// TestLocalDegradation_OptInResolvesToHealthyAlt verifies the P67 end-to-end
// opt-in path: the single node fails the requested (large) model but can
// serve the declared local alternate (small) just fine. With the retry
// budget exhausted for the primary model (RouteExcluding has nothing left to
// try - only one node, already tried) and the opt-in header present, the
// local degradation chain is walked before cloud, the alternate is served,
// the substitution is surfaced via the existing fallback header, and no
// cloud provider is ever contacted.
func TestLocalDegradation_OptInResolvesToHealthyAlt(t *testing.T) {
	var gotModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		gotModel.Store(model)
		if model == "big-model" {
			// Simulate a genuine upstream failure (not just a non-2xx status,
			// which httputil.ReverseProxy would pass through untouched) by
			// closing the connection before any response bytes are written -
			// this is what actually triggers ErrorHandler/retry.
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"small-model","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{
		LocalDegradationChains: map[string][]string{"big-model": {"small-model"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"big-model","prompt":"hi"}`)))
	req.Header.Set("X-Ollama-Mesh-Allow-Local-Degradation", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if want := "big-model -> small-model"; rec.Header().Get("X-Ollama-Mesh-Model-Fallback") != want {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want %q", rec.Header().Get("X-Ollama-Mesh-Model-Fallback"), want)
	}
	if got, _ := gotModel.Load().(string); got != "small-model" {
		t.Errorf("backend last received model = %q, want small-model", got)
	}
}

// TestLocalDegradation_NoOptInHeaderFallsToCloud verifies the header is a
// hard gate: with the identical fleet and chain as
// TestLocalDegradation_OptInResolvesToHealthyAlt, omitting the opt-in header
// must never substitute - the request proceeds to cloud fallback (or, with
// no cloud configured here, a clear upstream error) exactly as it would
// without P67 at all.
func TestLocalDegradation_NoOptInHeaderFallsToCloud(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		gotModel = model
		if model == "big-model" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"small-model","done":true}`))
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{
		LocalDegradationChains: map[string][]string{"big-model": {"small-model"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"big-model","prompt":"hi"}`)))
	// No X-Ollama-Mesh-Allow-Local-Degradation header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want a failure status - no opt-in header means no substitution and no cloud is configured")
	}
	if got := rec.Header().Get("X-Ollama-Mesh-Model-Fallback"); got != "" {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want empty (no opt-in header)", got)
	}
	if gotModel != "big-model" {
		t.Errorf("backend received model = %q, want big-model (no substitution attempted)", gotModel)
	}
}

// TestLocalDegradation_ChainExhaustedFallsToCloud verifies that when the
// fleet has no healthy node at all (the primary trigger site - WaitForNode
// already gave up), a declared local degradation chain is consulted but
// correctly finds nothing (every candidate is exhausted, same as the fleet
// state the primary model saw) and the request falls through to the existing
// 503 behavior rather than hanging or panicking.
func TestLocalDegradation_ChainExhaustedFallsToCloud(t *testing.T) {
	r := router.New(config.RoutingConfig{
		LocalDegradationChains: map[string][]string{"big-model": {"small-model"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false // fleet-wide outage: no candidate, primary or alternate, can be routed
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"big-model","prompt":"hi"}`)))
	req.Header.Set("X-Ollama-Mesh-Allow-Local-Degradation", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Ollama-Mesh-Model-Fallback"); got != "" {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want empty (chain exhausted, no substitution)", got)
	}
}

// TestLocalDegradation_CyclicChainDoesNotHang is a regression test: a
// two-entry cyclic chain (a -> b, b -> a) combined with a single always-
// failing node used to loop the retry loop indefinitely, because the
// degradation branch had no hop limit and never excluded already-tried
// nodes. Single-hop enforcement (degradedOnce) now bounds any request to at
// most one substitution regardless of chain shape, so this must terminate
// quickly with a failure status rather than hang.
func TestLocalDegradation_CyclicChainDoesNotHang(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{
		LocalDegradationChains: map[string][]string{
			"model-a": {"model-b"},
			"model-b": {"model-a"},
		},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"model-a","prompt":"hi"}`)))
	req.Header.Set("X-Ollama-Mesh-Allow-Local-Degradation", "true")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return within 10s - cyclic degradation chain is looping")
	}

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want a failure status; every node always fails, so no substitution can succeed")
	}
	if got := rec.Header().Get("X-Ollama-Mesh-Model-Fallback"); got != "model-a -> model-b" {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want exactly one hop (model-a -> model-b)", got)
	}
}

// TestLocalDegradation_RespectsPerKeyAllowList is a regression test: the
// per-key model allow-list was enforced only once, against the originally
// requested model, before routing - local degradation could substitute a
// model the key is not permitted to use. The substitution must now be
// skipped when the alternate is outside the key's allow-list.
func TestLocalDegradation_RespectsPerKeyAllowList(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{
		LocalDegradationChains: map[string][]string{"big-model": {"large-model"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader([]byte(`{"model":"big-model","prompt":"hi"}`)))
	req.Header.Set("X-Ollama-Mesh-Allow-Local-Degradation", "true")
	// Simulate the auth middleware having already run: this key is only
	// permitted to use "big-model", never "large-model".
	req = req.WithContext(context.WithValue(req.Context(), auth.AllowedModelsContextKey, []string{"big-model"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Ollama-Mesh-Model-Fallback"); got != "" {
		t.Errorf("X-Ollama-Mesh-Model-Fallback = %q, want empty - the only alternate is outside the key's allow-list", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want a failure status; the only local alternate is not in the key's allow-list")
	}
}

// TestAnthropicCompletionsTranslatedToMessages verifies that a
// /v1/completions request routed to an Anthropic overflow provider is
// translated to Anthropic's /v1/messages schema and actually proxied there
// (see cloudtranslate_anthropic_test.go for the translation's own coverage),
// rather than being rejected - Anthropic has no /v1/completions endpoint, but
// the mesh now speaks Messages on the provider's behalf.
func TestAnthropicCompletionsTranslatedToMessages(t *testing.T) {
	var gotPath string
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)) //nolint:errcheck
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

	if rec.Code != http.StatusOK {
		t.Fatalf("Anthropic /v1/completions: got %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("Anthropic backend received path %q, want /v1/messages", gotPath)
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
	audits := waitForAuditEntries(t, al, 1)
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

	audits := waitForAuditEntries(t, al, 1)
	if len(audits) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(audits))
	}
	if audits[0].CloudModel != "" {
		t.Errorf("audit cloud_model = %q, want empty when no rewrite", audits[0].CloudModel)
	}
}

// newLocalOnlyHandler mirrors newCloudFallbackHandler but additionally wires
// an auth.Middleware with the given keys, so localOnlyBlocked has a key to
// look up via auth.KeyNameFromContext. Returns the store too, so tests can
// assert on spill_counters directly (the P66 admin/CLI/UI read surfaces are
// exercised separately; this is the enforcement-path test).
func newLocalOnlyHandler(t *testing.T, cloud config.CloudProvider, keys []config.KeyConfig) (*Handler, store.Store) {
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
	tmpDB := filepath.Join(t.TempDir(), "local_only.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	a.SetStore(st)
	h := NewHandler(r, a, nil)
	h.SetAuth(auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: keys}))
	return h, st
}

// requestWithKeyName builds a request carrying keyName in context exactly as
// auth.Middleware would inject it after a successful key check, so the
// handler's auth.KeyNameFromContext lookup (and therefore localOnlyBlocked)
// sees the right key without needing a full auth round-trip in this test.
func requestWithKeyName(keyName string) *http.Request {
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	ctx := context.WithValue(req.Context(), auth.KeyNameContextKey, keyName)
	return req.WithContext(ctx)
}

// TestLocalOnlyBlockedWhenNoLocalNode guards the P66 fail-closed policy: a
// local_only key with no local node available must get a 503
// local_only_blocked error and must never reach the cloud provider, even
// though one is configured and would otherwise serve the request.
func TestLocalOnlyBlockedWhenNoLocalNode(t *testing.T) {
	cloudCalled := false
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudCalled = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[]}`))
	}))
	defer cloudSrv.Close()

	h, st := newLocalOnlyHandler(t, config.CloudProvider{
		Name: "fake-openai", Provider: "openai", BaseURL: cloudSrv.URL,
		APIKey: "test-key", Enabled: true,
	}, []config.KeyConfig{
		{Name: "finance", Key: "sk-finance", RateLimit: 100, LocalOnly: true},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithKeyName("finance"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "local_only_blocked") {
		t.Errorf("body = %q, want it to contain local_only_blocked", rec.Body.String())
	}
	if cloudCalled {
		t.Error("cloud provider must never be called for a local_only key")
	}

	rows, err := st.SpillCounters()
	if err != nil {
		t.Fatalf("SpillCounters: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.KeyName == "finance" && r.ServedBy == "blocked" && r.Requests == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a (finance, blocked, 1) spill_counters row, got: %+v", rows)
	}
}

// TestLocalOnlyFalseStillFallsBackToCloud is the regression guard from the
// P66 verification plan: a key with local_only=false (the default, i.e.
// every existing key today) must behave exactly as before - falling back to
// cloud and incrementing the provider's spill counter, never "blocked".
func TestLocalOnlyFalseStillFallsBackToCloud(t *testing.T) {
	cloudCalled := false
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudCalled = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test","choices":[],"usage":{"total_tokens":10}}`))
	}))
	defer cloudSrv.Close()

	h, st := newLocalOnlyHandler(t, config.CloudProvider{
		Name: "fake-openai", Provider: "openai", BaseURL: cloudSrv.URL,
		APIKey: "test-key", CostPer1KTokens: 0.002, Enabled: true,
	}, []config.KeyConfig{
		{Name: "default", Key: "sk-default", RateLimit: 100},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithKeyName("default"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cloud fallback should succeed)", rec.Code)
	}
	if !cloudCalled {
		t.Error("expected cloud provider to be called for a non-local_only key")
	}

	rows, err := st.SpillCounters()
	if err != nil {
		t.Fatalf("SpillCounters: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		if r.KeyName == "default" {
			got[r.ServedBy] = r.Requests
		}
	}
	if got["fake-openai"] != 1 {
		t.Errorf("expected 1 request counted for served_by=fake-openai, got rows: %+v", rows)
	}
	if got["blocked"] != 0 {
		t.Errorf("non-local_only key must never increment the blocked counter, got rows: %+v", rows)
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
