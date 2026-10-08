package proxy

// audit_review_test.go - follow-up regression tests: path forms, ambiguous
// model keys, header allow-listing to cloud providers, failover rate-cap
// skipping, fallback-chain controls, tail edges and abort recording.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

func TestManagementPathTrailingSlashFormsBlocked(t *testing.T) {
	for _, p := range []string{"/api/pull/", "/api/pull//", "/api/delete/", "/api/blobs/", "/api/blobs/sha256:x/", "/api/pull"} {
		if !isBlockedManagementPath(p) {
			t.Errorf("%q should be blocked", p)
		}
	}
	for _, p := range []string{"/api/generate/", "/api/tags", "/api/pulls", "/v1/models"} {
		if isBlockedManagementPath(p) {
			t.Errorf("%q must not be blocked", p)
		}
	}
}

func TestCanonicalPath(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"/":                  "/",
		"/api/generate":      "/api/generate",
		"/api/../api/pull":   "/api/pull",
		"//api/pull":         "/api/pull",
		"/api//pull/":        "/api/pull/",
		"/api/./generate":    "/api/generate",
		"/v1/chat/../models": "/v1/models",
		"/../x":              "/x",
	}
	for in, want := range cases {
		if got := canonicalPath(in); got != want {
			t.Errorf("canonicalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPercentEncodedTraversalWithRawPathIsBlockedAndNormalized(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	for _, p := range []string{"/api/../api/pull", "/api/pull/", "/api/pull//"} {
		req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m"}`))
		req.URL.Path = p
		req.URL.RawPath = "/api/%2e%2e/api/pull"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with RawPath: status %d, want 403", p, rec.Code)
		}
	}
}

func TestHasAmbiguousModelKey(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"model":"a"}`, false},
		{`{"model":"a","options":{"model":"x"}}`, false},
		{`{"messages":[{"model":"x"}],"model":"a"}`, false},
		{`[{"model":"a","MODEL":"b"}]`, false},
		{`not json`, false},
		{``, false},
		{`{"model":"a","model":"a"}`, true},
		{`{"model":"a","MODEL":"b"}`, true},
		{`{"mOdEl":"a"}`, true},
		{`{"model":"a","model":"b"}`, true},
		{`{"MODEL":"a"}`, true},
	}
	for _, c := range cases {
		if got := hasAmbiguousModelKey([]byte(c.body)); got != c.want {
			t.Errorf("hasAmbiguousModelKey(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}

func TestDisallowedModelMetricLabelIsFixed(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	authMW := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{
		{Name: "k", Key: "sk-k", RateLimit: 1000, Models: []string{"allowed"}},
	}})
	h.SetAuth(authMW)
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"attacker-chosen-label-xyz"}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	rec := httptest.NewRecorder()
	authMW.Handler(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sawFixed := false
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), "attacker-chosen-label-xyz") {
					t.Errorf("client-chosen model name became a metric label on %s", f.GetName())
				}
				if l.GetName() == "model" && l.GetValue() == "disallowed" {
					sawFixed = true
				}
			}
		}
	}
	if !sawFixed {
		t.Error("expected a requests_total sample with the fixed model label \"disallowed\"")
	}
}

func TestCloudRequestForGuardsEmptyClientModel(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	out, model := cloudRequestFor(body, "local-x", "", false)
	if string(out) != string(body) {
		t.Errorf("body rewritten to %s; an absent client model must not become an empty \"model\"", out)
	}
	if model != "local-x" {
		t.Errorf("model = %q, want unchanged", model)
	}
}

func TestCloudHeadersAreAllowListed(t *testing.T) {
	cloud := newCloudMock(t)
	r := router.New(config.RoutingConfig{}, nil, []config.CloudProvider{
		{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk-cloud", Priority: 1, Enabled: true},
	})
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range map[string]string{
		"Cookie": "marbor_session=s", "Forwarded": "for=1.2.3.4", "Via": "1.1 proxy",
		"True-Client-IP": "1.2.3.4", "CF-Connecting-IP": "1.2.3.4", "X-Client-IP": "1.2.3.4",
		"X-Forwarded-Host": "h", "X-Forwarded-Proto": "https", "X-Forwarded-Port": "443",
		"Referer": "http://r", "Origin": "http://o", "X-Marbor-Debug": "1",
		"OpenAI-Organization": "org-x", "OpenAI-Project": "proj-x", "Anthropic-Beta": "b",
		"User-Agent": "secret-client-agent", "X-Request-ID": "client-chosen-id",
	} {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)

	_, hdr := cloud.snapshot()
	if hdr == nil {
		t.Fatal("cloud never contacted")
	}
	for _, k := range []string{"Cookie", "Forwarded", "Via", "True-Client-Ip", "Cf-Connecting-Ip", "X-Client-Ip",
		"X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Forwarded-For", "Referer", "Origin", "X-Marbor-Debug",
		"Openai-Organization", "Openai-Project", "Anthropic-Beta"} {
		if v := hdr.Get(k); v != "" {
			t.Errorf("cloud received %s = %q", k, v)
		}
	}
	if strings.Contains(hdr.Get("User-Agent"), "secret-client-agent") {
		t.Errorf("client User-Agent reached the cloud: %q", hdr.Get("User-Agent"))
	}
	if hdr.Get("X-Request-Id") == "client-chosen-id" || hdr.Get("X-Request-Id") == "" {
		t.Errorf("X-Request-ID = %q, want marbor's own id", hdr.Get("X-Request-Id"))
	}
	if hdr.Get("Content-Type") != "application/json" || hdr.Get("Accept") != "application/json" {
		t.Errorf("Content-Type/Accept lost: %v", hdr)
	}
	if hdr.Get("Authorization") != "Bearer sk-cloud" {
		t.Errorf("Authorization = %q", hdr.Get("Authorization"))
	}
}

func TestConnectionTokenCannotStripCloudCredential(t *testing.T) {
	cloud := newCloudMock(t)
	r := router.New(config.RoutingConfig{}, nil, []config.CloudProvider{
		{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk-cloud", Priority: 1, Enabled: true},
	})
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Connection", "Authorization, X-Request-ID")
	h.ServeHTTP(httptest.NewRecorder(), req)
	_, hdr := cloud.snapshot()
	if hdr == nil {
		t.Fatal("cloud never contacted")
	}
	if hdr.Get("Authorization") != "Bearer sk-cloud" {
		t.Errorf("Authorization = %q; a Connection token stripped marbor's credential", hdr.Get("Authorization"))
	}
	if hdr.Get("X-Request-Id") == "" {
		t.Error("X-Request-ID stripped by a Connection token")
	}
}

func TestConnectionTokenCannotStripLocalRequestID(t *testing.T) {
	var gotID atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID.Store(r.Header.Get("X-Request-ID"))
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "n", URL: upstream.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`))
	req.Header.Set("Connection", "X-Request-ID")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if id, _ := gotID.Load().(string); id == "" {
		t.Error("backend received no X-Request-ID; a Connection token stripped it")
	}
}

// newCapRig builds nodes a (dead, warm for m), b (healthy, rpm cap spent) and
// c (healthy, uncapped) so a failover has to skip b.
func newCapRig(t *testing.T, includeC bool, hits map[string]*int32) (*Handler, *admin.Server, *router.Router) {
	t.Helper()
	mk := func(name string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(hits[name], 1)
			w.Write([]byte(`{"done":true}`))
		}))
		t.Cleanup(s.Close)
		return s
	}
	nodes := []config.NodeConfig{
		{Name: "a", URL: deadURL(t), Runtime: "ollama"},
		{Name: "b", URL: mk("b").URL, Runtime: "ollama"},
	}
	if includeC {
		nodes = append(nodes, config.NodeConfig{Name: "c", URL: mk("c").URL, Runtime: "ollama"})
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first", MaxRetries: 3}, nodes, nil)
	for _, n := range r.Nodes() {
		if n.Name == "a" {
			seedNode(n, "m")
		} else {
			seedNode(n, "")
		}
	}
	st, err := store.Open(t.TempDir() + "/cap.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	one := 1
	if err := st.SetModelConfig(store.ModelConfig{Model: "m", Node: "b", RPM: &one}); err != nil {
		t.Fatal(err)
	}
	a := admin.NewServer(r, nil, config.Config{}, st)
	h := NewHandler(r, a, nil)
	h.modelLimiter.allow("m", "b", &one, nil) // spend b's only request
	return h, a, r
}

func TestFailoverSkipsNodeWithExhaustedModelCap(t *testing.T) {
	hits := map[string]*int32{"b": new(int32), "c": new(int32)}
	h, _, r := newCapRig(t, true, hits)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d (%s), want 200 served by the uncapped node", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(hits["b"]) != 0 || atomic.LoadInt32(hits["c"]) != 1 {
		t.Errorf("hits b=%d c=%d, want b skipped and c served", *hits["b"], *hits["c"])
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

func TestFailoverWithOnlyCappedNodeKeepsOriginalFailure(t *testing.T) {
	hits := map[string]*int32{"b": new(int32)}
	h, _, r := newCapRig(t, false, hits)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502 (the original upstream failure), not a 429 about the skipped node", rec.Code)
	}
	if atomic.LoadInt32(hits["b"]) != 0 {
		t.Error("capped node was contacted")
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

func TestInitialRateLimit429HidesNodeNameAndReleasesCounters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "gpu-secret-7", URL: upstream.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	st, err := store.Open(t.TempDir() + "/c.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	one := 1
	if err := st.SetModelConfig(store.ModelConfig{Model: "m", Node: "gpu-secret-7", RPM: &one}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}, st), nil)
	h.modelLimiter.allow("m", "gpu-secret-7", &one, nil)
	buf := captureLog(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "gpu-secret-7") {
		t.Errorf("429 body names the node: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "gpu-secret-7") {
		t.Errorf("node name should be logged server-side, log: %q", buf.String())
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

func TestInvalidNodeURLReleasesCounters(t *testing.T) {
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "n", URL: "http://bad host", Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 for an unparseable node URL", rec.Code)
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

func TestNormalCompletionReleasesCounters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"done":true,"eval_count":2}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "n", URL: upstream.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)))
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

func TestClientCancelBeforeResponseIsLoggedWithoutFabricated200(t *testing.T) {
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "n", URL: deadURL(t), Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)).WithContext(ctx))
	es := liveEntries(t, a)
	if len(es) != 1 {
		t.Fatalf("entries = %d, want 1", len(es))
	}
	if es[0].HTTPStatus == 200 {
		t.Errorf("a request cancelled before any response was logged as HTTP 200")
	}
	if es[0].Status != "aborted" {
		t.Errorf("status = %q, want aborted", es[0].Status)
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

// fallbackRig serves tags for the 70b model plus alt1 and alt2 (both small) and
// records the model each inference request carried.
func fallbackRig(t *testing.T, chain []string, keys []config.KeyConfig) (http.Handler, *atomic.Value, *Handler) {
	t.Helper()
	var got atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"big","size":41943040000},{"name":"alt1","size":4194304000},{"name":"alt2","size":4194304000}]}`))
			return
		}
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		got.Store(b["model"])
		w.Write([]byte(`{"done":true}`))
	}))
	t.Cleanup(upstream.Close)
	r := router.New(config.RoutingConfig{Strategy: "warm-first", FallbackChains: map[string][]string{"big": chain}},
		[]config.NodeConfig{{Name: "gpu-0", URL: upstream.URL, GPUModel: "V100", Runtime: "ollama", VRAMTotalMB: 8192}}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.VRAMTotalMB = 8192
		n.Unlock()
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	authMW := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: keys})
	h.SetAuth(authMW)
	return authMW.Handler(h), &got, h
}

func TestFallbackChainPositiveAndAllowListControls(t *testing.T) {
	cases := []struct {
		name   string
		models []string
		want   string
	}{
		{"unrestricted key falls back", nil, "alt1"},
		{"allow-list containing alt falls back", []string{"big", "alt1"}, "alt1"},
		{"first alt disallowed picks the second", []string{"big", "alt2"}, "alt2"},
		{"all alts disallowed means no swap", []string{"big"}, "big"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, got, _ := fallbackRig(t, []string{"alt1", "alt2"}, []config.KeyConfig{{Name: "k", Key: "sk-k", RateLimit: 1000, Models: c.models}})
			req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"big","prompt":"x"}`))
			req.Header.Set("Authorization", "Bearer sk-k")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if g, _ := got.Load().(string); g != c.want {
				t.Errorf("backend received %q, want %q (status %d)", g, c.want, rec.Code)
			}
		})
	}
}

// countLine builds a JSON line of exactly total bytes (including the newline
// when nl is true) whose final fields carry eval_count=7, prompt_eval_count=3.
func countLine(total int, nl bool) []byte {
	head, tail := `{"response":"`, `","eval_count":7,"prompt_eval_count":3}`
	n := total - len(head) - len(tail)
	if nl {
		n--
	}
	s := head + strings.Repeat("x", n) + tail
	if nl {
		s += "\n"
	}
	return []byte(s)
}

func TestTokenCountTailBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		total int
		nl    bool
		want  int64
	}{
		{"exactly tailMax", tailMax, true, 10},
		{"tailMax plus one", tailMax + 1, true, 10},
		{"exactly embedTailMax", embedTailMax, true, 10},
		{"embedTailMax plus one", embedTailMax + 1, true, -1},
		{"no trailing newline, large", 20 * 1024, false, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
			rec.Write(countLine(c.total, c.nl))
			if got := rec.tokenCount(rec.truncatedTail); got != c.want {
				t.Errorf("tokenCount = %d, want %d", got, c.want)
			}
		})
	}
}

func TestTokenCountSSEUsageLineSurvivesTrailingDone(t *testing.T) {
	usage := `data: {"id":"x","pad":"` + strings.Repeat("u", 3*tailMax) + `","usage":{"total_tokens":42}}` + "\n\n"
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
	rec.Write([]byte(usage))
	rec.Write([]byte("data: [DONE]\n\n"))
	if got := rec.tokenCount(rec.truncatedTail); got != 42 {
		t.Errorf("tokenCount = %d, want 42 (usage line must survive the trailing [DONE])", got)
	}

	huge := `data: {"pad":"` + strings.Repeat("u", embedTailMax+10) + `","usage":{"total_tokens":42}}` + "\n\n"
	rec = &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte(huge))
	rec.Write([]byte("data: [DONE]\n\n"))
	if got := rec.tokenCount(rec.truncatedTail); got != -1 {
		t.Errorf("tokenCount = %d, want -1 for a usage line too large to retain", got)
	}
}

func TestTokenCountUnparseableLastLineDoesNotFallBackToEarlierLine(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte(`{"eval_count":7,"prompt_eval_count":3}` + "\n" + `{"respon`))
	if got := rec.tokenCount(false); got != -1 {
		t.Errorf("tokenCount = %d, want -1 when the last JSON line cannot be parsed", got)
	}
}

func TestAbortedStreamIsRecordedBeforeClientSeesIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"response":"partial","done":false}`+"\n")
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "n", URL: upstream.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	var access strings.Builder
	h.SetAccessLogger(NewAccessLogger(&access, true))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"m","prompt":"x"}`))
	if err == nil {
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("client saw a clean end of stream")
	}
	es := liveEntries(t, a)
	if len(es) != 1 || es[0].Status != "aborted" {
		t.Errorf("request log = %+v, want one aborted entry recorded before the abort reached the client", es)
	}
	if !strings.Contains(access.String(), `"model":"m"`) {
		t.Errorf("access log empty or missing the request: %q", access.String())
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

func TestCloudAbortIsRecordedAndKeepsUnknownTokenCount(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[]}`+"\n")
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer cloud.Close()
	r := router.New(config.RoutingConfig{}, nil, []config.CloudProvider{
		{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "k", Priority: 1, Enabled: true},
	})
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	if err == nil {
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("client saw a clean end of a dead cloud stream")
	}
	es := liveEntries(t, a)
	if len(es) != 1 || es[0].Status != "aborted" {
		t.Fatalf("request log = %+v, want one aborted entry", es)
	}
	if es[0].Tokens != -1 {
		t.Errorf("tokens = %d, want -1 (unknown) passed through unchanged", es[0].Tokens)
	}
}

func TestRecoverMiddlewareNonCanonicalProxyPathUsesEnvelope(t *testing.T) {
	h := RecoverMiddleware(panicHandler("boom"))
	req := httptest.NewRequest("POST", "/v1/x", nil)
	req.URL.Path = "//v1/x"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"type"`) {
		t.Errorf("want OpenAI envelope for //v1/x, got %q", rec.Body.String())
	}
}
