package proxy

import (
	"bytes"
	"encoding/json"
	"io"
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

const (
	aliasName   = "gpt-4"
	aliasTarget = "llama3.2:8b"
)

// modelRecorder is a fake backend node that records the model field of every
// inference request it receives, answers /api/tags from catalog, and replies
// with a small JSON body.
type modelRecorder struct {
	mu     sync.Mutex
	models []string
	paths  []string
}

func (m *modelRecorder) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.models) == 0 {
		return ""
	}
	return m.models[len(m.models)-1]
}

func newModelRecorderNode(t *testing.T, catalog ...string) (*httptest.Server, *modelRecorder) {
	t.Helper()
	rec := &modelRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			type tag struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}
			out := struct {
				Models []tag `json:"models"`
			}{}
			for _, c := range catalog {
				out.Models = append(out.Models, tag{Name: c, Size: 1 << 20})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		model, _ := body["model"].(string)
		rec.mu.Lock()
		rec.models = append(rec.models, model)
		rec.paths = append(rec.paths, r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"` + model + `","done":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newAliasHandler wires a Handler over one healthy node with the given alias
// map and returns the router and admin server for inspection.
func newAliasHandler(t *testing.T, nodeURL string, routing config.RoutingConfig, cfg config.Config, st ...store.Store) (*Handler, *router.Router, *admin.Server) {
	t.Helper()
	if routing.Strategy == "" {
		routing.Strategy = "warm-first"
	}
	if routing.ModelAliases == nil {
		routing.ModelAliases = map[string]string{aliasName: aliasTarget}
	}
	r := router.New(routing, []config.NodeConfig{
		{Name: "gpu-0", URL: nodeURL, GPUModel: "V100", Runtime: "ollama", VRAMTotalMB: 65536},
	}, cfg.CloudProviders)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.VRAMTotalMB = 65536
		n.Unlock()
	}
	a := admin.NewServer(r, nil, cfg, st...)
	return NewHandler(r, a, nil), r, a
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAlias_OllamaPath_BodyRewrittenToTarget(t *testing.T) {
	node, got := newModelRecorderNode(t)
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{})

	for _, path := range []string{"/api/chat", "/api/generate", "/api/embed", "/api/embeddings"} {
		rec := serve(h, http.MethodPost, path, `{"model":"gpt-4","prompt":"hi","stream":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, rec.Code, rec.Body.String())
		}
		if m := got.last(); m != aliasTarget {
			t.Errorf("%s: upstream saw model %q, want %q", path, m, aliasTarget)
		}
	}
}

func TestAlias_OpenAIPath_BodyRewrittenToTarget(t *testing.T) {
	node, got := newModelRecorderNode(t)
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{})

	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		rec := serve(h, http.MethodPost, path, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, rec.Code, rec.Body.String())
		}
		if m := got.last(); m != aliasTarget {
			t.Errorf("%s: upstream saw model %q, want %q", path, m, aliasTarget)
		}
	}
}

func TestAlias_Streaming_ChunksFlushIncrementally(t *testing.T) {
	const interChunkDelay = 50 * time.Millisecond
	chunks := []string{
		`{"model":"llama3.2:8b","response":"a","done":false}` + "\n",
		`{"model":"llama3.2:8b","response":"b","done":false}` + "\n",
		`{"done":true,"eval_count":10,"prompt_eval_count":2}` + "\n",
	}
	var sawModel atomic.Value
	var mockDone atomic.Int64
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		sawModel.Store(body["model"])
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i, c := range chunks {
			if i > 0 {
				time.Sleep(interChunkDelay)
			}
			io.WriteString(w, c)
			f.Flush()
		}
		mockDone.Store(time.Now().UnixNano())
	}))
	defer node.Close()
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{})

	rec := newStreamRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"gpt-4"}`)))

	if sawModel.Load() != aliasTarget {
		t.Fatalf("upstream saw %v, want %q", sawModel.Load(), aliasTarget)
	}
	if got, want := rec.buf.String(), strings.Join(chunks, ""); got != want {
		t.Fatalf("body = %q, want %q (response must pass through unchanged)", got, want)
	}
	if len(rec.writeTimes) < 2 {
		t.Fatalf("got %d writes, want >= 2 (aliased response was buffered)", len(rec.writeTimes))
	}
	if gap := rec.writeTimes[len(rec.writeTimes)-1].Sub(rec.writeTimes[0]); gap < interChunkDelay {
		t.Errorf("first-to-last write gap = %v, want >= %v", gap, interChunkDelay)
	}
	if !rec.writeTimes[0].Before(time.Unix(0, mockDone.Load())) {
		t.Error("first client write happened after the node finished - response was buffered")
	}
	if rec.flushes == 0 {
		t.Error("Flush never reached the client")
	}
}

func TestAlias_HeaderAndLoggedModel(t *testing.T) {
	node, _ := newModelRecorderNode(t)
	h, _, a := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{})

	rec := serve(h, http.MethodPost, "/api/generate", `{"model":"gpt-4","prompt":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("X-Marbor-Model-Alias"), "gpt-4 -> llama3.2:8b"; got != want {
		t.Errorf("X-Marbor-Model-Alias = %q, want %q", got, want)
	}
	if got := rec.Header().Get("X-Marbor-Model-Fallback"); got != "" {
		t.Errorf("X-Marbor-Model-Fallback = %q, want empty (an alias is not a fallback)", got)
	}
	entries := fetchRequestEntries(t, a)
	if len(entries) != 1 || entries[0].Model != "gpt-4 -> llama3.2:8b" {
		t.Fatalf("request log = %+v, want model %q", entries, "gpt-4 -> llama3.2:8b")
	}
}

func TestAlias_ThenFallbackChain_KeyedByTarget(t *testing.T) {
	var gotModel atomic.Value
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[
				{"name":"llama3.1:70b","size":41943040000},
				{"name":"llama3.1:70b-q4_K_M","size":4194304000}
			]}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotModel.Store(body["model"])
		w.Write([]byte(`{"done":true}`))
	}))
	defer node.Close()

	r := router.New(config.RoutingConfig{
		Strategy:       "warm-first",
		ModelAliases:   map[string]string{aliasName: "llama3.1:70b"},
		FallbackChains: map[string][]string{"llama3.1:70b": {"llama3.1:70b-q4_K_M"}},
	}, []config.NodeConfig{{Name: "gpu-0", URL: node.URL, Runtime: "ollama", VRAMTotalMB: 8192}}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.VRAMTotalMB = 8192
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	rec := serve(h, http.MethodPost, "/api/generate", `{"model":"gpt-4","prompt":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if gotModel.Load() != "llama3.1:70b-q4_K_M" {
		t.Fatalf("upstream saw %v, want the fallback of the alias target", gotModel.Load())
	}
	if got, want := rec.Header().Get("X-Marbor-Model-Fallback"), "llama3.1:70b -> llama3.1:70b-q4_K_M"; got != want {
		t.Errorf("fallback header = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("X-Marbor-Model-Alias"), "gpt-4 -> llama3.1:70b"; got != want {
		t.Errorf("alias header = %q, want %q", got, want)
	}
	entries := fetchRequestEntries(t, a)
	if want := "gpt-4 -> llama3.1:70b -> llama3.1:70b-q4_K_M"; len(entries) != 1 || entries[0].Model != want {
		t.Fatalf("request log = %+v, want %q", entries, want)
	}
}

func TestAlias_ContextWindowAndModelConfigKeyedByTarget(t *testing.T) {
	// Context window declared for the target only: an oversized aliased
	// request must be rejected against the target's window.
	node, got := newModelRecorderNode(t)
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{ContextWindows: map[string]int{aliasTarget: 4}})
	rec := serve(h, http.MethodPost, "/api/generate", `{"model":"gpt-4","prompt":"`+strings.Repeat("x", 200)+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), aliasTarget) {
		t.Fatalf("status = %d body = %s, want 400 naming the target's context window", rec.Code, rec.Body.String())
	}

	// Model config profile declared for (target, node): its defaults must be
	// injected into an aliased request.
	st, err := store.Open(filepath.Join(t.TempDir(), "mc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetModelConfig(store.ModelConfig{Model: aliasTarget, Node: "gpu-0", Temperature: fp(0.25)}); err != nil {
		t.Fatal(err)
	}
	var sawTemp atomic.Value
	node2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if opts, ok := body["options"].(map[string]any); ok {
			sawTemp.Store(opts["temperature"])
		}
		w.Write([]byte(`{"done":true}`))
	}))
	defer node2.Close()
	h2, _, _ := newAliasHandler(t, node2.URL, config.RoutingConfig{}, config.Config{}, st)
	if rec := serve(h2, http.MethodPost, "/api/generate", `{"model":"gpt-4","prompt":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if sawTemp.Load() != 0.25 {
		t.Fatalf("temperature injected = %v, want 0.25 from the target's profile", sawTemp.Load())
	}
	_ = got
}

func TestAlias_AllowList(t *testing.T) {
	node, got := newModelRecorderNode(t)
	cases := []struct {
		name    string
		allowed []string
		want    int
	}{
		{"alias listed only", []string{aliasName}, http.StatusOK},
		{"target listed only", []string{aliasTarget}, http.StatusOK},
		{"neither listed", []string{"something-else"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authMw := auth.NewMiddleware(config.AuthConfig{
				Enabled: config.BoolPtr(true),
				Keys:    []config.KeyConfig{{Name: "k", Key: "sk-k", RateLimit: 2, Models: tc.allowed}},
			})
			h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{})
			h.SetAuth(authMw)
			wrapped := authMw.Handler(h)
			// Five requests against rate_limit=2: a rejected request must be
			// refunded every time, so it stays 403 and never turns into 429.
			n := 1
			if tc.want == http.StatusForbidden {
				n = 5
			}
			for i := 0; i < n; i++ {
				req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"gpt-4","prompt":"hi"}`))
				req.Header.Set("Authorization", "Bearer sk-k")
				rec := httptest.NewRecorder()
				wrapped.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("request %d: status = %d, want %d: %s", i+1, rec.Code, tc.want, rec.Body.String())
				}
				if tc.want == http.StatusForbidden {
					body := rec.Body.String()
					if !strings.Contains(body, aliasName) || strings.Contains(body, aliasTarget) {
						t.Fatalf("403 body = %s, want it to name the requested alias and never the target", body)
					}
					if hv := rec.Header().Get("X-Marbor-Model-Alias"); hv != "" {
						t.Fatalf("403 carries X-Marbor-Model-Alias = %q, want no header so the target is not revealed", hv)
					}
				}
			}
			if tc.want == http.StatusOK && got.last() != aliasTarget {
				t.Fatalf("upstream saw %q, want %q", got.last(), aliasTarget)
			}
		})
	}
}

// newAliasCloudProvider is a fake OpenAI-compatible provider recording the
// model field it RECEIVED on the wire.
func newAliasCloudProvider(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var received atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		received.Store(body["model"])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","model":"cloud-echo","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &received
}

func TestAlias_CloudFallbackModelName(t *testing.T) {
	t.Run("no local node sends the client name", func(t *testing.T) {
		cloud, received := newAliasCloudProvider(t)
		cfg := config.Config{CloudProviders: []config.CloudProvider{{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk", Enabled: true}}}
		h, r, _ := newAliasHandler(t, "http://127.0.0.1:1", config.RoutingConfig{}, cfg)
		for _, n := range r.Nodes() {
			n.Lock()
			n.Healthy = false
			n.Unlock()
		}
		rec := serve(h, http.MethodPost, "/api/chat", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if received.Load() != aliasName {
			t.Fatalf("cloud received model %v, want %q", received.Load(), aliasName)
		}
		// A cloud-translated /api/* response reports the requested name.
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if out["model"] != aliasName {
			t.Fatalf("translated response model = %v, want %q", out["model"], aliasName)
		}
	})

	t.Run("default_model still overrides", func(t *testing.T) {
		cloud, received := newAliasCloudProvider(t)
		cfg := config.Config{CloudProviders: []config.CloudProvider{{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk", Enabled: true, DefaultModel: "gpt-4o"}}}
		h, r, _ := newAliasHandler(t, "http://127.0.0.1:1", config.RoutingConfig{}, cfg)
		for _, n := range r.Nodes() {
			n.Lock()
			n.Healthy = false
			n.Unlock()
		}
		if rec := serve(h, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4","messages":[]}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if received.Load() != "gpt-4o" {
			t.Fatalf("cloud received %v, want the provider default_model", received.Load())
		}
	})

	t.Run("client name survives a local degradation swap", func(t *testing.T) {
		cloud, received := newAliasCloudProvider(t)
		var localSaw sync.Map
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			localSaw.Store(body["model"], true)
			// Fail before any response bytes so the retry/degradation path runs.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}))
		defer node.Close()
		cfg := config.Config{CloudProviders: []config.CloudProvider{{Name: "c", Provider: "openai", BaseURL: cloud.URL, APIKey: "sk", Enabled: true}}}
		h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{
			LocalDegradationChains: map[string][]string{aliasTarget: {"small-model"}},
		}, cfg)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[]}`))
		req = withKeyName(h, req, "k", []config.KeyConfig{{Name: "k", Key: "k", AllowLocalDegradation: true}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if _, ok := localSaw.Load("small-model"); !ok {
			t.Fatal("expected the local degradation swap to be attempted first")
		}
		if received.Load() != aliasName {
			t.Fatalf("cloud received %v after a swap, want the client name %q", received.Load(), aliasName)
		}
	})
}

func TestAlias_NotAppliedToManagementPaths(t *testing.T) {
	node, got := newModelRecorderNode(t)
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{}, config.Config{})
	h.SetAllowManagementEndpoints(true)

	// A normal aliased request first, so the alias is demonstrably active.
	if rec := serve(h, http.MethodPost, "/api/generate", `{"model":"gpt-4","prompt":"hi"}`); rec.Code != http.StatusOK || got.last() != aliasTarget {
		t.Fatalf("aliased inference: status %d, upstream saw %q", rec.Code, got.last())
	}
	for _, path := range []string{"/api/pull", "/api/push", "/api/create", "/api/copy", "/api/delete", "/api/blobs/sha256:abc"} {
		method := http.MethodPost
		if path == "/api/delete" {
			method = http.MethodDelete
		}
		rec := serve(h, method, path, `{"model":"gpt-4"}`)
		if hdr := rec.Header().Get("X-Marbor-Model-Alias"); hdr != "" {
			t.Errorf("%s: alias header %q set on a management path", path, hdr)
		}
		if m := got.last(); m != aliasName {
			t.Errorf("%s: node saw model %q, want the literal %q", path, m, aliasName)
		}
	}
}

func TestAlias_SessionAffinityAndRoutingKeyedByTarget(t *testing.T) {
	nodeA, sawA := newModelRecorderNode(t)
	nodeB, sawB := newModelRecorderNode(t)
	r := router.New(config.RoutingConfig{
		Strategy:        "warm-first",
		SessionAffinity: true,
		ModelAliases:    map[string]string{aliasName: aliasTarget},
	}, []config.NodeConfig{
		{Name: "a", URL: nodeA.URL, Runtime: "ollama"},
		{Name: "b", URL: nodeB.URL, Runtime: "ollama"},
	}, nil)
	// Node a has a model literally named "gpt-4" warm; node b has the target
	// warm. The alias always wins, so routing must pick b.
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		if n.Name == "a" {
			n.LoadedModels = []router.ModelInfo{{Name: aliasName}}
		} else {
			n.LoadedModels = []router.ModelInfo{{Name: aliasTarget}}
		}
		n.Unlock()
	}
	h := NewHandler(r, admin.NewServer(r, nil, config.Config{}), nil)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"gpt-4","prompt":"hi"}`))
		req.Header.Set("X-Session-ID", "s1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	}
	sawA.mu.Lock()
	aCount := len(sawA.models)
	sawA.mu.Unlock()
	sawB.mu.Lock()
	bCount := len(sawB.models)
	sawB.mu.Unlock()
	if aCount != 0 || bCount != 3 {
		t.Fatalf("node a served %d, node b served %d; want every aliased request on the target's warm node", aCount, bCount)
	}
}

func TestServeModels_ListsAliasWithTargetStatus(t *testing.T) {
	node, _ := newModelRecorderNode(t, aliasTarget, "mistral:7b", "qwen2.5:7b")
	h, r, _ := newAliasHandler(t, node.URL, config.RoutingConfig{ModelAliases: map[string]string{
		aliasName:     aliasTarget,  // target loaded
		"gpt-4o-mini": "qwen2.5:7b", // target downloaded only
		"claude":      "absent",     // target not on the fleet: not listed
		"mistral:7b":  aliasTarget,  // shadows a real model: one row, target's status
	}}, config.Config{})
	for _, n := range r.Nodes() {
		n.Lock()
		n.LoadedModels = []router.ModelInfo{{Name: aliasTarget}}
		n.Unlock()
	}
	rec := serve(h, http.MethodGet, "/v1/models", "")
	var out struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	counts := map[string]int{}
	status := map[string]string{}
	for _, d := range out.Data {
		counts[d.ID]++
		status[d.ID] = d.Status
	}
	want := map[string]string{
		aliasName:     "loaded",
		aliasTarget:   "loaded",
		"gpt-4o-mini": "available",
		"qwen2.5:7b":  "available",
		"mistral:7b":  "loaded",
	}
	for id, st := range want {
		if counts[id] != 1 || status[id] != st {
			t.Errorf("%s: count=%d status=%q, want exactly one row with %q", id, counts[id], status[id], st)
		}
	}
	if counts["claude"] != 0 {
		t.Errorf("alias with an absent target must not be listed")
	}
	if len(out.Data) != len(want) {
		t.Errorf("rows = %+v, want %d", out.Data, len(want))
	}
}

func TestServeModel_AliasLookup(t *testing.T) {
	node, _ := newModelRecorderNode(t, "qwen2.5:7b", "mistral:7b")
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{ModelAliases: map[string]string{
		aliasName:    "qwen2.5:7b",
		"mistral:7b": "absent", // shadows a real model whose target is unavailable
	}}, config.Config{})

	rec := serve(h, http.MethodGet, "/v1/models/gpt-4", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"gpt-4"`) || !strings.Contains(rec.Body.String(), `"status":"available"`) {
		t.Fatalf("GET /v1/models/gpt-4 = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, http.MethodGet, "/v1/models/mistral:7b", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("shadowing alias with absent target = %d, want 404 even though the real model exists", rec.Code)
	}
	if rec := serve(h, http.MethodGet, "/v1/models/qwen2.5:7b", ""); rec.Code != http.StatusOK {
		t.Fatalf("real model lookup = %d, want 200", rec.Code)
	}
}

func TestNoAliases_BehaviorUnchanged(t *testing.T) {
	node, got := newModelRecorderNode(t, "llama3")
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "gpu-0", URL: node.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)
	rec := serve(h, http.MethodPost, "/api/generate", `{"model":"llama3","prompt":"hi"}`)
	if rec.Code != http.StatusOK || got.last() != "llama3" {
		t.Fatalf("status %d, upstream saw %q", rec.Code, got.last())
	}
	if hdr := rec.Header().Get("X-Marbor-Model-Alias"); hdr != "" {
		t.Fatalf("alias header %q set with no aliases configured", hdr)
	}
	if entries := fetchRequestEntries(t, a); len(entries) != 1 || entries[0].Model != "llama3" {
		t.Fatalf("request log = %+v", entries)
	}
	models := serve(h, http.MethodGet, "/v1/models", "")
	if !bytes.Contains(models.Body.Bytes(), []byte(`"id":"llama3"`)) {
		t.Fatalf("/v1/models = %s", models.Body.String())
	}
}

func TestAlias_HeaderValueIsExactAndSafe(t *testing.T) {
	// Validation rejects control characters, so the header value an alias
	// produces is always a single clean line.
	if err := config.ValidateModelAliases(map[string]string{"x\r\nSet-Cookie: a=b": "y"}); err == nil {
		t.Fatal("CRLF alias must be rejected before it can reach a response header")
	}
	node, _ := newModelRecorderNode(t)
	h, _, _ := newAliasHandler(t, node.URL, config.RoutingConfig{ModelAliases: map[string]string{"org/model:tag": aliasTarget}}, config.Config{})
	rec := serve(h, http.MethodPost, "/v1/chat/completions", `{"model":"org/model:tag","messages":[]}`)
	if got := rec.Header().Get("X-Marbor-Model-Alias"); got != "org/model:tag -> llama3.2:8b" {
		t.Fatalf("header = %q", got)
	}
}
