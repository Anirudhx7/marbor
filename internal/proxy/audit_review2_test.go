package proxy

// audit_review2_test.go - regression tests for body-shape validation, metric
// label bounding, log truncation, cloud query handling, degradation chain
// caps, whole-document token counts and request Content-Encoding.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

func newEmptyFleetHandler() *Handler {
	r := router.New(config.RoutingConfig{}, nil, nil)
	return NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
}

func TestDeeplyNestedBodyIsRejectedWithoutCrashing(t *testing.T) {
	body := `{"model":"m","a":` + strings.Repeat("[", 5<<20)
	if !hasAmbiguousModelKey([]byte(body)) {
		t.Error("a 5 MiB nest of arrays should be flagged")
	}
	rec := httptest.NewRecorder()
	newEmptyFleetHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
	nested := `{"model":"m","a":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`
	if hasAmbiguousModelKey([]byte(nested)) {
		t.Error("200 levels of nesting is legitimate")
	}
}

func TestMalformedObjectBodyIsRejectedButBodylessRequestsAreNot(t *testing.T) {
	h := newEmptyFleetHandler()
	for _, body := range []string{`{"model":"m",`, `{"model":"m"} trailing`, `{"model":}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/generate", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, rec.Code)
		}
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/tags", ""},
		{"POST", "/api/generate", ""},
		{"POST", "/api/generate", `[1,2]`},
		{"POST", "/api/generate", `{"model":"m"}  `},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rec.Code == http.StatusBadRequest {
			t.Errorf("%s %s body %q must not be rejected as malformed", c.method, c.path, c.body)
		}
	}
}

func TestUnknownModelNamesShareOneMetricLabel(t *testing.T) {
	h := newEmptyFleetHandler()
	for i := 0; i < 6; i++ {
		name := "zz-unk-label-" + strings.Repeat("x", i+1)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+name+`"}`)))
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	sawUnknown := false
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.HasPrefix(l.GetValue(), "zz-unk-label-") {
					t.Errorf("client-chosen model %q became a label on %s", l.GetValue(), f.GetName())
				}
				if l.GetName() == "model" && l.GetValue() == "unknown" {
					sawUnknown = true
				}
			}
		}
	}
	if !sawUnknown {
		t.Error("expected unknown model names to share the \"unknown\" label")
	}
}

func TestLoggedPathsAreTruncated(t *testing.T) {
	buf := captureLog(t)
	h := newEmptyFleetHandler()
	long := strings.Repeat("a", 5000)
	for _, p := range []string{"/api/blobs/" + long, "/v1//" + long} {
		req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m"}`))
		req.URL.Path = p
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if len(line) > 600 && !strings.Contains(line, "WARNING") {
			t.Errorf("log line of %d bytes: client path was not truncated", len(line))
		}
	}
}

func TestCloudRequestDropsClientQueryString(t *testing.T) {
	cloud := newCloudMock(t)
	r := router.New(config.RoutingConfig{}, nil, []config.CloudProvider{
		{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "k", Priority: 1, Enabled: true},
	})
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions?api_key=secret&x=1", strings.NewReader(`{"model":"gpt-4o","messages":[]}`)))
	if body, _ := cloud.snapshot(); body == nil {
		t.Fatal("cloud never contacted")
	}
	if q := cloud.lastQuery(); q != "" {
		t.Errorf("cloud received query %q", q)
	}
}

func TestRequestContentEncodingIsRejected(t *testing.T) {
	h := newEmptyFleetHandler()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("\x1f\x8b not really gzip"))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status %d, want 415", rec.Code)
	}
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Content-Encoding", "identity")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Error("identity encoding must be accepted")
	}
}

func TestPrettyPrintedReplyKeepsTokenCountAndDurations(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte("{\n  \"model\": \"m\",\n  \"eval_count\": 7,\n  \"prompt_eval_count\": 3,\n  \"eval_duration\": 5000000,\n  \"prompt_eval_duration\": 2000000\n}\n"))
	if got := rec.tokenCount(false); got != 10 {
		t.Errorf("tokenCount = %d, want 10", got)
	}
	if got := rec.evalDurationMs(); got != 5 {
		t.Errorf("evalDurationMs = %d, want 5", got)
	}
	if got := rec.promptEvalDurationMs(); got != 2 {
		t.Errorf("promptEvalDurationMs = %d, want 2", got)
	}
}

func TestLongLineGrownByManySmallWritesKeepsCount(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte("{\"a\":1}\n"))
	rec.Write([]byte(`{"response":"` + strings.Repeat("x", 3*tailMax)))
	for i := 0; i < 200; i++ {
		rec.Write([]byte(strings.Repeat("y", 50)))
	}
	rec.Write([]byte(`","eval_count":7,"prompt_eval_count":3}` + "\n"))
	if got := rec.tokenCount(rec.truncatedTail); got != 10 {
		t.Errorf("tokenCount = %d, want 10", got)
	}
	if len(rec.tail) > embedTailMax {
		t.Errorf("tail grew to %d, past embedTailMax", len(rec.tail))
	}
}

func TestDegradationTriesLaterChainEntryWhenFirstIsCapped(t *testing.T) {
	var gotModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		if b["model"] == "model-a" {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		gotModel.Store(b["model"])
		w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	r := router.New(config.RoutingConfig{LocalDegradationChains: map[string][]string{"model-a": {"model-b", "model-c"}}},
		[]config.NodeConfig{{Name: "gpu-0", URL: upstream.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}
	st, err := store.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	one := 1
	if err := st.SetModelConfig(store.ModelConfig{Model: "model-b", Node: "gpu-0", RPM: &one}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}, st), nil)
	h.modelLimiter.allow("model-b", "gpu-0", &one, nil)

	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"model-a","prompt":"hi"}`))
	req = withKeyName(h, req, "k", []config.KeyConfig{{Name: "k", Key: "k", AllowLocalDegradation: true}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if g, _ := gotModel.Load().(string); g != "model-c" || rec.Code != 200 {
		t.Errorf("served model %q status %d, want model-c 200 (model-b capped)", g, rec.Code)
	}
	if got := rec.Header().Get("X-Marbor-Model-Fallback"); got != "model-a -> model-c" {
		t.Errorf("fallback header %q", got)
	}
	if got := activeConns(r); got != 0 {
		t.Errorf("ActiveConns = %d", got)
	}
}
