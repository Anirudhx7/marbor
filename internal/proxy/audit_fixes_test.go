package proxy

// audit_fixes_test.go - regression tests for a batch of proxy hardening fixes:
// fallback-chain allow-list, cancel accounting, per-node model profiles,
// model-name validation, token-count tail, header hygiene to cloud providers,
// transport timeouts, path canonicalisation and abort propagation.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

const fallbackTagsJSON = `{"models":[
	{"name":"llama3.1:70b","size":41943040000},
	{"name":"llama3.1:70b-q4_K_M","size":4194304000}
]}`

// newTightVRAMRouter builds a router with one 8 GiB node (the 70b model does
// not fit, the q4 alternate does) and a fallback chain from 70b to q4.
func newTightVRAMRouter(upstreamURL string, clouds []config.CloudProvider) *router.Router {
	r := router.New(config.RoutingConfig{
		Strategy:       "warm-first",
		FallbackChains: map[string][]string{"llama3.1:70b": {"llama3.1:70b-q4_K_M"}},
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: upstreamURL, GPUModel: "V100", Runtime: "ollama", VRAMTotalMB: 8192},
	}, clouds)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.VRAMTotalMB = 8192
		n.VRAMUsedMB = 0
		n.Unlock()
	}
	return r
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestFallbackChainSkipsAltOutsideKeyAllowList(t *testing.T) {
	var gotModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(fallbackTagsJSON))
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		gotModel.Store(body["model"])
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()

	r := newTightVRAMRouter(upstream.URL, nil)
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	authMW := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{
		{Name: "restricted", Key: "sk-restricted", RateLimit: 1000, Models: []string{"llama3.1:70b"}},
	}})
	h.SetAuth(authMW)

	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"llama3.1:70b","prompt":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-restricted")
	rec := httptest.NewRecorder()
	authMW.Handler(h).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Marbor-Model-Fallback"); got != "" {
		t.Errorf("fallback header = %q, want none: the alternate is not in the key's model list", got)
	}
	if got, _ := gotModel.Load().(string); got != "llama3.1:70b" {
		t.Errorf("backend received model %q, want the requested llama3.1:70b", got)
	}
}

// cloudMock records the last request it received.
type cloudMock struct {
	*httptest.Server
	mu      sync.Mutex
	body    map[string]interface{}
	headers http.Header
}

func newCloudMock(t *testing.T) *cloudMock {
	t.Helper()
	m := &cloudMock{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		m.mu.Lock()
		m.body = b
		m.headers = r.Header.Clone()
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":3}}`))
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *cloudMock) snapshot() (map[string]interface{}, http.Header) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.body, m.headers
}

func TestFallbackChainHandoffToCloudSendsClientModelAndNoFallbackHeader(t *testing.T) {
	// The node serves /api/tags but drops the inference connection before any
	// response byte, so the request fails over to the cloud provider.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(fallbackTagsJSON))
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer upstream.Close()
	cloud := newCloudMock(t)

	r := newTightVRAMRouter(upstream.URL, []config.CloudProvider{
		{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk-c", Priority: 1, Enabled: true},
	})
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"llama3.1:70b","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body, _ := cloud.snapshot()
	if body == nil {
		t.Fatalf("cloud never contacted; status %d body %s", rec.Code, rec.Body.String())
	}
	if body["model"] != "llama3.1:70b" {
		t.Errorf("cloud received model %v, want the client's llama3.1:70b", body["model"])
	}
	if got := rec.Header().Get("X-Marbor-Model-Fallback"); got != "" {
		t.Errorf("fallback header %q leaked onto a cloud response", got)
	}
}

func TestCloudRequestDoesNotForwardClientIdentityHeaders(t *testing.T) {
	cloud := newCloudMock(t)
	r := router.New(config.RoutingConfig{}, nil, []config.CloudProvider{
		{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk-cloud", Priority: 1, Enabled: true},
	})
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Accept-Encoding", "br")
	req.Header.Set("X-Api-Key", "client-secret")
	req.Header.Set("X-Session-Id", "sess-1")
	req.Header.Set("Proxy-Authorization", "Basic abc")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	_, hdr := cloud.snapshot()
	if hdr == nil {
		t.Fatalf("cloud never contacted; status %d", rec.Code)
	}
	for _, k := range []string{"X-Api-Key", "X-Session-Id", "Proxy-Authorization", "X-Forwarded-For", "X-Real-Ip", "Forwarded"} {
		if v := hdr.Get(k); v != "" {
			t.Errorf("cloud received %s = %q, want it stripped", k, v)
		}
	}
	if v := hdr.Get("Accept-Encoding"); v == "br" {
		t.Errorf("cloud received the client's Accept-Encoding %q", v)
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-cloud" {
		t.Errorf("Authorization = %q, want the provider key", got)
	}
}

func TestClientCancelDoesNotMarkNodeFailed(t *testing.T) {
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: deadURL(t), Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	n := r.Nodes()[0]
	n.RLock()
	defer n.RUnlock()
	if !n.LastErrorAt.IsZero() {
		t.Errorf("client cancellation put the node into error cooldown (LastErrorAt=%v)", n.LastErrorAt)
	}
	if len(n.SuccessHistory) != 0 {
		t.Errorf("client cancellation recorded an outcome for the node: %v", n.SuccessHistory)
	}
}

func TestClientDisconnectMidStreamDoesNotMarkNodeFailed(t *testing.T) {
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		for i := 0; i < 300; i++ {
			if _, err := io.WriteString(w, `{"response":"tok","done":false}`+"\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer upstream.Close()

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"m","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // client walks away mid-stream

	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the abort")
	}
	n := r.Nodes()[0]
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&n.ActiveConns) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The request log entry is written after the outcome is recorded, so its
	// presence proves the handler finished recording.
	waitForLiveEntries(t, a, 1)
	n.RLock()
	defer n.RUnlock()
	if !n.LastErrorAt.IsZero() {
		t.Errorf("client disconnect put the node into error cooldown (LastErrorAt=%v)", n.LastErrorAt)
	}
}

// newProfileRetryRig builds a dead node "a" (warm for model m, so it is picked
// first) and a healthy node "b" with store-backed model profiles.
func newProfileRetryRig(t *testing.T, good *httptest.Server, cfgs ...store.ModelConfig) *Handler {
	t.Helper()
	r := router.New(config.RoutingConfig{Strategy: "warm-first", MaxRetries: 1}, []config.NodeConfig{
		{Name: "a", URL: deadURL(t), Runtime: "ollama"},
		{Name: "b", URL: good.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		if n.Name == "a" {
			seedNode(n, "m")
		} else {
			seedNode(n, "")
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, c := range cfgs {
		if err := st.SetModelConfig(c); err != nil {
			t.Fatal(err)
		}
	}
	return NewHandler(r, admin.NewServer(r, nil, config.Config{}, st), nil)
}

func TestRetryNodeGetsItsOwnModelProfile(t *testing.T) {
	var gotTemp atomic.Value
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		if opts, ok := b["options"].(map[string]interface{}); ok {
			gotTemp.Store(opts["temperature"])
		}
		w.Write([]byte(`{"done":true}`))
	}))
	defer good.Close()
	t01, t09 := 0.1, 0.9
	h := newProfileRetryRig(t, good,
		store.ModelConfig{Model: "m", Node: "a", Temperature: &t01},
		store.ModelConfig{Model: "m", Node: "b", Temperature: &t09})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := gotTemp.Load(); got != 0.9 {
		t.Errorf("retry node received temperature %v, want its own profile value 0.9", got)
	}
}

func TestRetryNodeEnforcesItsOwnRateLimit(t *testing.T) {
	var hits int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"done":true}`))
	}))
	defer good.Close()
	one := 1
	h := newProfileRetryRig(t, good, store.ModelConfig{Model: "m", Node: "b", RPM: &one})
	// Use up node b's one request for this minute.
	h.modelLimiter.allow("m", "b", &one, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	// A failover node whose cap is spent is skipped, so the client sees the
	// original upstream failure rather than a 429 about a node it never chose.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (retry node skipped for its rpm cap)", rec.Code)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("retry node was contacted despite its rate cap")
	}
}

func TestModelNameLengthCap(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	long := strings.Repeat("a", 257)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+long+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("257-byte model name: status %d, want 400", rec.Code)
	}
	var env apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Message == "" {
		t.Errorf("want an OpenAI error envelope, got %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+strings.Repeat("a", 256)+`"}`)))
	if rec.Code == http.StatusBadRequest {
		t.Errorf("256-byte model name must be accepted, got 400")
	}
}

func TestAmbiguousModelKeysRejected(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "allowed")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	authMW := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{
		{Name: "k", Key: "sk-k", RateLimit: 1000, Models: []string{"allowed"}},
	}})
	h.SetAuth(authMW)
	srv := authMW.Handler(h)

	post := func(body string) int {
		req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-k")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, body := range []string{
		`{"model":"secret","MODEL":"allowed"}`,
		`{"MODEL":"allowed","model":"secret"}`,
		`{"Model":"allowed"}`,
		`{"model":"secret","model":"allowed"}`,
	} {
		if code := post(body); code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, code)
		}
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("ambiguous body reached a backend")
	}
	if code := post(`{"model":"allowed","prompt":"x","modelx":1}`); code != http.StatusOK {
		t.Errorf("unambiguous body: status %d, want 200", code)
	}
}

func TestRewriteModelFieldDropsCaseVariants(t *testing.T) {
	out := rewriteModelField([]byte(`{"model":"a","MODEL":"b","Model":"c","x":1}`), "z")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["model"] != "z" || m["x"] != float64(1) {
		t.Errorf("rewrite left case-variant keys behind: %s", out)
	}
}

func TestTokenCountSingleLongLineWithTrailingNewline(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	pad := strings.Repeat("x", 20*1024)
	line := `{"response":"` + pad + `","done":true,"eval_count":7,"prompt_eval_count":3}` + "\n"
	rec.Write([]byte(line))
	if got := rec.tokenCount(rec.truncatedTail); got != 10 {
		t.Errorf("tokenCount = %d, want 10", got)
	}
}

func TestTokenCountOversizedFinalLineIsUnknownNotZero(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	pad := strings.Repeat("x", 2*embedTailMax)
	rec.Write([]byte(`{"response":"` + pad + `","eval_count":7}` + "\n"))
	if got := rec.tokenCount(rec.truncatedTail); got != -1 {
		t.Errorf("tokenCount = %d, want -1 (unknown) for a line too large to retain", got)
	}
}

func TestTokenCountUsageLineAfterLongLines(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	for i := 0; i < 5; i++ {
		rec.Write([]byte(`{"response":"` + strings.Repeat("y", 6000) + `"}` + "\n"))
	}
	rec.Write([]byte(`{"done":true,"eval_count":4,"prompt_eval_count":1}` + "\n"))
	if got := rec.tokenCount(false); got != 5 {
		t.Errorf("tokenCount = %d, want 5", got)
	}
}

func TestErrorRequestLatencyIsRealElapsedTime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))

	req := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	req.AddCookie(&http.Cookie{Name: "marbor_session", Value: a.AdminToken()})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	var entries []struct {
		HTTPStatus int `json:"httpStatus"`
		Latency    int `json:"latency"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil || len(entries) != 1 {
		t.Fatalf("live entries: %v %v", entries, err)
	}
	if entries[0].HTTPStatus != 500 || entries[0].Latency < 50 {
		t.Errorf("entry = %+v, want a 500 with real latency >= 50ms", entries[0])
	}
}

func TestBlockedPathLogEscapesControlCharacters(t *testing.T) {
	buf := captureLog(t)

	r := router.New(config.RoutingConfig{}, nil, nil)
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	req := httptest.NewRequest("POST", "/api/blobs/x", nil)
	req.URL.Path = "/api/blobs/x\nFORGED line"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "FORGED") {
			t.Errorf("a decoded newline in the path started a forged log line: %q", buf.String())
		}
	}
	if !strings.Contains(buf.String(), `path="/api/blobs/x\nFORGED line"`) {
		t.Errorf("path not logged quoted: %q", buf.String())
	}
}

func TestLocalTransportHasDialTimeoutAndIdleLimits(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	tr := h.localRoundTripper()
	if tr.DialContext == nil {
		t.Fatal("local transport has no DialContext (no connect timeout)")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("local transport has no TLS handshake timeout")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("local transport has no idle connection timeout")
	}
	if tr.MaxIdleConnsPerHost < 8 {
		t.Errorf("MaxIdleConnsPerHost = %d, want a pool of at least 8", tr.MaxIdleConnsPerHost)
	}
	// A refused dial must fail promptly through the configured dialer.
	l, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatalf("listen: %v", lerr)
	}
	addr := l.Addr().String()
	l.Close()
	if _, err := tr.DialContext(context.Background(), "tcp", addr); err == nil {
		t.Error("dial to a closed port unexpectedly succeeded")
	}
}

func TestUnsupportedOpenAIPathsCoverUnderscoreSpellings(t *testing.T) {
	for _, p := range []string{
		"/v1/fine_tuning/jobs", "/v1/vector_stores", "/v1/vector_stores/vs_1/files",
		"/v1/uploads", "/v1/uploads/up_1/parts", "/v1/responses", "/v1/evals", "/v1/batches",
		"/v1/fine-tuning/jobs", "/v1/vector-stores",
	} {
		if !isUnsupportedOpenAIPath(p) {
			t.Errorf("%s should be reported unsupported", p)
		}
	}
	for _, p := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/models"} {
		if isUnsupportedOpenAIPath(p) {
			t.Errorf("%s must not be reported unsupported", p)
		}
	}
}

func TestPathTraversalFormsOfBlockedEndpointsAreBlocked(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	for _, p := range []string{"/api/../api/pull", "//api/pull", "/api/./delete", "/api//push", "/v1/../api/create", "/api/blobs/../blobs/sha256:x"} {
		req := httptest.NewRequest("POST", "/api/pull", strings.NewReader(`{"model":"m"}`))
		req.URL.Path = p
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", p, rec.Code)
		}
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("blocked path variants reached the backend")
	}
}

func TestNonCanonicalPathIsForwardedCleaned(t *testing.T) {
	var gotPath atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`))
	req.URL.Path = "/api/./x/../generate"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got, _ := gotPath.Load().(string); got != "/api/generate" {
		t.Errorf("backend saw path %q, want the cleaned /api/generate", got)
	}
}

// TestMidStreamUpstreamDeathReachesClientAsAbort proves a backend that dies
// mid-body is surfaced to the client as an aborted stream, not as a clean end
// of a chunked response.
func TestMidStreamUpstreamDeathReachesClientAsAbort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"response":"partial","done":false}`+"\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close() // no terminating chunk: an abrupt death
		}
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"m","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("client read %q with a nil error; want an abort error", data)
	}
}

type panicWriter struct{ http.ResponseWriter }

func (panicWriter) Write([]byte) (int, error) { panic("writer exploded") }

func TestPanicDuringProxyDoesNotLeakConnectionCounters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n", URL: upstream.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(panicWriter{httptest.NewRecorder()}, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	}()

	if got := atomic.LoadInt32(&r.Nodes()[0].ActiveConns); got != 0 {
		t.Errorf("ActiveConns = %d after a panic, want 0", got)
	}
}

func liveEntries(t *testing.T, a *admin.Server) []struct {
	Status     string `json:"status"`
	HTTPStatus int    `json:"httpStatus"`
	Tokens     int64  `json:"tokens"`
	Latency    int    `json:"latency"`
} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	req.AddCookie(&http.Cookie{Name: "marbor_session", Value: a.AdminToken()})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	var entries []struct {
		Status     string `json:"status"`
		HTTPStatus int    `json:"httpStatus"`
		Tokens     int64  `json:"tokens"`
		Latency    int    `json:"latency"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode live requests: %v", err)
	}
	return entries
}

// waitForLiveEntries polls until the request log holds at least want entries.
func waitForLiveEntries(t *testing.T, a *admin.Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(liveEntries(t, a)) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("request log never reached %d entries", want)
}

func activeConns(r *router.Router) int32 {
	var total int32
	for _, n := range r.Nodes() {
		total += atomic.LoadInt32(&n.ActiveConns)
	}
	return total
}
