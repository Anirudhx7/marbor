package proxy

import (
	"bytes"
	"context"
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

// coOpts configures one cloud-only test environment: a loopback fake cloud, a
// loopback fake local backend and a handler wired to both.
type coOpts struct {
	list          []string
	noCloud       bool
	cloudProvider string // "" = openai
	cloudMode     string // "" = ok, "close" = drop the connection, "partial" = headers then drop
	runtime       string // node runtime, "" = ollama
	loaded        []string
	state         string // "saturated" or ""
	aliases       map[string]string
	fallback      map[string][]string
	degradation   map[string][]string
	failModels    []string // local backend drops the connection for these models
	tags          string   // local /api/tags body
	vramMB        int64
	localOnly     bool
	allowDegrade  bool
	dailyCap      float64
	withBudget    bool
	allowMgmt     bool
	allowed       []string
	ctxWindows    map[string]int
	timeoutMs     int
	slaMs         int
}

type coEnv struct {
	h         *Handler
	r         *router.Router
	st        store.Store
	node      *router.NodeState
	cloudHits atomic.Int32
	localHits atomic.Int32
	mu        sync.Mutex
	cloudSeen []string
	allowed   []string
	localSeen []string
	rowsSeen  int
}

func (e *coEnv) cloudModels() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.cloudSeen...)
}

func (e *coEnv) localModels() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.localSeen...)
}

func bodyModel(raw []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Model
}

func newCOEnv(t *testing.T, o coOpts) *coEnv {
	t.Helper()
	e := &coEnv{}
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		e.cloudHits.Add(1)
		e.mu.Lock()
		e.cloudSeen = append(e.cloudSeen, bodyModel(raw))
		e.mu.Unlock()
		switch o.cloudMode {
		case "close", "partial":
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			if o.cloudMode == "partial" {
				buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
				buf.Flush()
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"c","choices":[],"usage":{"total_tokens":10}}`))
	}))
	t.Cleanup(cloudSrv.Close)
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/tags" && o.tags != "" {
			w.Write([]byte(o.tags))
			return
		}
		raw, _ := io.ReadAll(req.Body)
		model := bodyModel(raw)
		e.localHits.Add(1)
		e.mu.Lock()
		e.localSeen = append(e.localSeen, model)
		e.mu.Unlock()
		for _, f := range o.failModels {
			if f == model {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close()
					}
				}
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"l","choices":[],"done":true}`))
	}))
	t.Cleanup(localSrv.Close)

	var clouds []config.CloudProvider
	if !o.noCloud {
		provider := o.cloudProvider
		if provider == "" {
			provider = "openai"
		}
		clouds = []config.CloudProvider{{Name: "fake-cloud", Provider: provider, BaseURL: cloudSrv.URL, APIKey: "k", CostPer1KTokens: 0.002, Enabled: true}}
	}
	vram := o.vramMB
	if o.runtime == "" {
		o.runtime = "ollama"
	}
	r := router.New(config.RoutingConfig{
		Strategy:               "warm-first",
		MaxInFlightPerNode:     1,
		CloudOnlyModels:        o.list,
		ModelAliases:           o.aliases,
		FallbackChains:         o.fallback,
		LocalDegradationChains: o.degradation,
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: localSrv.URL, GPUModel: "V100", Runtime: o.runtime, VRAMTotalMB: vram},
	}, clouds)
	timeout, sla := o.timeoutMs, o.slaMs
	if timeout == 0 {
		timeout = 300
	}
	if sla == 0 {
		sla = 100
	}
	r.SetQueueMaxDepth(10)
	r.SetQueueTimeout(timeout)
	r.SetOverflowSLA(sla)
	e.r = r
	e.node = r.Nodes()[0]
	e.node.Lock()
	e.node.LastPollAt = time.Now()
	e.node.Healthy = true
	if vram > 0 {
		e.node.VRAMTotalMB = vram
	}
	for _, m := range o.loaded {
		e.node.LoadedModels = append(e.node.LoadedModels, router.ModelInfo{Name: m, SizeVRAM: 1024})
	}
	e.node.Unlock()
	if o.state == "saturated" {
		r.IncrConn(e.node)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "co.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	a := admin.NewServer(r, nil, config.Config{
		CloudBudget:    config.CloudBudgetConfig{DailyUSDCap: o.dailyCap},
		ContextWindows: o.ctxWindows,
	}, st)
	t.Cleanup(a.Shutdown)
	if o.withBudget {
		a.TrackCloudCostModelNoSpill("k", "openai", "gpt-4o", o.dailyCap*2, 1000)
	}
	h := NewHandler(r, a, nil)
	h.SetAllowManagementEndpoints(o.allowMgmt)
	h.SetAuth(auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{
		{Name: "k", Key: "sk-k", RateLimit: 1000, LocalOnly: o.localOnly, AllowLocalDegradation: o.allowDegrade},
	}}))
	e.h = h
	e.allowed = o.allowed
	return e
}

func (e *coEnv) serve(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	ctx := context.WithValue(req.Context(), auth.KeyNameContextKey, "k")
	if len(e.allowed) > 0 {
		ctx = context.WithValue(ctx, auth.AllowedModelsContextKey, e.allowed)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *coEnv) chat(model string) *httptest.ResponseRecorder {
	return e.serve("POST", "/v1/chat/completions", `{"model":"`+model+`","messages":[]}`)
}

func (e *coEnv) lastRow(t *testing.T) store.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Wait for a row beyond the ones already returned, so a second call
		// after a second request cannot hand back the first request's row.
		if recs, err := e.st.LastRequests(1000); err == nil && len(recs) > e.rowsSeen {
			e.rowsSeen = len(recs)
			return recs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no request_log row written")
	return store.RequestRecord{}
}

func (e *coEnv) spillRows(t *testing.T) []store.SpillCounterRow {
	t.Helper()
	rows, err := e.st.SpillCounters()
	if err != nil {
		t.Fatalf("SpillCounters: %v", err)
	}
	return rows
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("response is not an API error: %v: %s", err, rec.Body.String())
	}
	return b.Error.Code
}

func TestProxy_CloudOnly_GoesToCloudNotLocal(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
	rec := e.chat("paid-model")
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
		t.Fatalf("status=%d cloud=%d local=%d, want 200 served by cloud only", rec.Code, e.cloudHits.Load(), e.localHits.Load())
	}
	if rec.Header().Get("X-Marbor-Model-Alias") != "" || rec.Header().Get("X-Marbor-Model-Fallback") != "" {
		t.Errorf("alias/fallback headers = %q/%q, want none on a cloud-only reply", rec.Header().Get("X-Marbor-Model-Alias"), rec.Header().Get("X-Marbor-Model-Fallback"))
	}
	row := e.lastRow(t)
	if row.RoutingReason != CloudReasonCloudOnlyPolicy {
		t.Errorf("routing_reason = %q, want %q", row.RoutingReason, CloudReasonCloudOnlyPolicy)
	}
	if row.QueueWaitMs == nil || *row.QueueWaitMs != 0 {
		t.Errorf("queue_wait_ms = %v, want a real 0", row.QueueWaitMs)
	}
	if row.CloudMs == nil {
		t.Error("cloud_ms is nil, want it measured")
	}
	if !strings.Contains(row.RoutingDetail, "cloud-only") {
		t.Errorf("routing_detail = %q, want the cloud-only sentence", row.RoutingDetail)
	}
}

func TestProxy_CloudOnly_NoSpillWhenServed(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}})
	if rec := e.chat("paid-model"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	_ = e.lastRow(t)
	if rows := e.spillRows(t); len(rows) != 0 {
		t.Errorf("spill rows = %+v, want none: cloud-only traffic is not overflow", rows)
	}
	buckets, err := e.st.HourlyBuckets(time.Now().Add(-2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var cloudReqs int
	var cost float64
	for _, b := range buckets {
		cloudReqs += b.CloudRequests
		cost += b.CostUSD
	}
	if cloudReqs != 1 || cost <= 0 {
		t.Errorf("cloud requests=%d cost=%v, want the cost and budget bucket still recorded", cloudReqs, cost)
	}

	// Control: ordinary overflow still bumps the spill row.
	e2 := newCOEnv(t, coOpts{})
	e2.node.Lock()
	e2.node.Healthy = false
	e2.node.Unlock()
	if rec := e2.chat("other-model"); rec.Code != http.StatusOK {
		t.Fatalf("control status = %d, want 200 via overflow", rec.Code)
	}
	if rows := e2.spillRows(t); len(rows) != 1 {
		t.Errorf("control spill rows = %+v, want one overflow row", rows)
	}
}

func TestProxy_CloudOnly_SkipsQueueAndChains(t *testing.T) {
	e := newCOEnv(t, coOpts{
		list:         []string{"paid-model"},
		state:        "saturated",
		timeoutMs:    5000,
		slaMs:        4000,
		fallback:     map[string][]string{"paid-model": {"alt-model"}},
		degradation:  map[string][]string{"paid-model": {"alt-model"}},
		allowDegrade: true,
		ctxWindows:   map[string]int{"paid-model": 10},
	})
	big := strings.Repeat("x", 4000)
	start := time.Now()
	rec := e.serve("POST", "/v1/chat/completions", `{"model":"paid-model","messages":[{"role":"user","content":"`+big+`"}]}`)
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
		t.Fatalf("status=%d cloud=%d local=%d body=%s, want served by cloud with no context-window 400", rec.Code, e.cloudHits.Load(), e.localHits.Load(), rec.Body.String())
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v, want no queue wait", d)
	}
	if rec.Header().Get("X-Marbor-Model-Fallback") != "" {
		t.Error("fallback header present, want none")
	}
	if got := e.cloudModels(); len(got) != 1 || got[0] != "paid-model" {
		t.Errorf("cloud saw models %v, want [paid-model]", got)
	}
}

func TestProxy_CloudOnly_ChainSubstituteNotListed(t *testing.T) {
	t.Run("vram fallback chain", func(t *testing.T) {
		e := newCOEnv(t, coOpts{
			list:     []string{"listed-q4"},
			vramMB:   8192,
			fallback: map[string][]string{"big": {"listed-q4", "other-q4"}},
			tags: `{"models":[
				{"name":"big","size":41943040000},
				{"name":"listed-q4","size":4194304000},
				{"name":"other-q4","size":4194304000}]}`,
		})
		rec := e.serve("POST", "/api/generate", `{"model":"big","prompt":"hi"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		if want := "big -> other-q4"; rec.Header().Get("X-Marbor-Model-Fallback") != want {
			t.Errorf("fallback header = %q, want %q (the listed alternate skipped, the walk continues)", rec.Header().Get("X-Marbor-Model-Fallback"), want)
		}
		for _, m := range e.localModels() {
			if m == "listed-q4" {
				t.Fatal("a cloud-only model ran locally as a chain substitute")
			}
		}
	})
	t.Run("degradation chain with no node", func(t *testing.T) {
		e := newCOEnv(t, coOpts{
			list:         []string{"listed-small"},
			runtime:      "vllm",
			loaded:       []string{"listed-small", "other-small"},
			degradation:  map[string][]string{"big": {"listed-small", "other-small"}},
			allowDegrade: true,
		})
		rec := e.chat("big")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		if got := e.localModels(); len(got) != 1 || got[0] != "other-small" {
			t.Fatalf("local backend saw %v, want only [other-small]", got)
		}
	})
	t.Run("degradation chain in the retry loop", func(t *testing.T) {
		e := newCOEnv(t, coOpts{
			list:         []string{"listed-small"},
			degradation:  map[string][]string{"big": {"listed-small", "other-small"}},
			allowDegrade: true,
			failModels:   []string{"big"},
		})
		rec := e.serve("POST", "/api/generate", `{"model":"big","prompt":"hi"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		for _, m := range e.localModels() {
			if m == "listed-small" {
				t.Fatal("a cloud-only model ran locally as a degradation substitute")
			}
		}
		if got := e.localModels(); len(got) == 0 || got[len(got)-1] != "other-small" {
			t.Fatalf("local backend saw %v, want the walk to end on other-small", got)
		}
	})
}

func TestProxy_CloudOnly_AliasTarget(t *testing.T) {
	e := newCOEnv(t, coOpts{
		list:    []string{"real-model"},
		aliases: map[string]string{"gpt-x": "real-model"},
		loaded:  []string{"real-model"},
	})
	rec := e.chat("gpt-x")
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
		t.Fatalf("status=%d cloud=%d local=%d, want cloud only for an alias to a listed model", rec.Code, e.cloudHits.Load(), e.localHits.Load())
	}
	if got := e.cloudModels(); len(got) != 1 || got[0] != "gpt-x" {
		t.Errorf("cloud saw models %v, want the client-sent name gpt-x", got)
	}
	if rec.Header().Get("X-Marbor-Model-Alias") != "" {
		t.Error("alias header present on a cloud-only reply, want none")
	}
}

func TestProxy_CloudOnly_LocalOnlyKey_503NoCloud(t *testing.T) {
	for name, list := range map[string][]string{"exact": {"paid-model"}, "prefix": {"paid-*"}} {
		t.Run(name, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: list, localOnly: true, loaded: []string{"paid-model"}})
			rec := e.chat("paid-model")
			if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != CloudReasonLocalOnlyBlocked {
				t.Fatalf("status=%d body=%s, want 503 local_only_blocked", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "cloud-only list and this key is local_only") {
				t.Errorf("message = %s, want the cloud-only specific wording", rec.Body.String())
			}
			if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
				t.Errorf("cloud=%d local=%d, want neither contacted", e.cloudHits.Load(), e.localHits.Load())
			}
			row := e.lastRow(t)
			if row.RoutingReason != CloudReasonLocalOnlyBlocked || row.StatusCode != 503 || row.NodeName != "none" {
				t.Errorf("row reason=%q status=%d node=%q, want local_only_blocked/503/none", row.RoutingReason, row.StatusCode, row.NodeName)
			}
			if row.CloudMs != nil {
				t.Errorf("cloud_ms = %v, want nil", *row.CloudMs)
			}
			if rows := e.spillRows(t); len(rows) != 0 {
				t.Errorf("spill rows = %+v, want none: nothing overflowed", rows)
			}
		})
	}
}

func TestProxy_CloudOnly_NoCloudChain_503NoLocal(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, noCloud: true, loaded: []string{"paid-model"}})
	rec := e.chat("paid-model")
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "cloud_only_no_provider" {
		t.Fatalf("status=%d body=%s, want 503 cloud_only_no_provider", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no cloud provider is enabled") {
		t.Errorf("message = %s, want the no-provider sentence", rec.Body.String())
	}
	if e.localHits.Load() != 0 {
		t.Errorf("local backend hit %d times, want 0: never run locally", e.localHits.Load())
	}
	row := e.lastRow(t)
	if row.RoutingReason != CloudReasonCloudOnlyPolicy || row.StatusCode != 503 || row.NodeName != "none" {
		t.Errorf("row reason=%q status=%d node=%q, want cloud_only_policy/503/none", row.RoutingReason, row.StatusCode, row.NodeName)
	}
	if row.CloudMs != nil {
		t.Errorf("cloud_ms = %v, want nil", *row.CloudMs)
	}
	if rows := e.spillRows(t); len(rows) != 0 {
		t.Errorf("spill rows = %+v, want none", rows)
	}
}

func TestProxy_CloudOnly_BudgetExceeded_503NoLocal(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}, dailyCap: 1, withBudget: true})
	rec := e.chat("paid-model")
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "cloud_budget_exceeded" {
		t.Fatalf("status=%d body=%s, want 503 cloud_budget_exceeded", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the model is cloud-only and is not run locally") {
		t.Errorf("body = %s, want the cloud-only suffix", rec.Body.String())
	}
	if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
		t.Errorf("cloud=%d local=%d, want neither contacted", e.cloudHits.Load(), e.localHits.Load())
	}
	if rows := e.spillRows(t); len(rows) != 0 {
		t.Errorf("spill rows = %+v, want none", rows)
	}
	row := e.lastRow(t)
	if row.RoutingReason != CloudReasonCloudOnlyPolicy || row.StatusCode != 503 || row.NodeName != "none" {
		t.Errorf("row reason=%q status=%d node=%q, want cloud_only_policy/503/none like the other refusals", row.RoutingReason, row.StatusCode, row.NodeName)
	}
	if row.CloudMs != nil {
		t.Errorf("cloud_ms = %v, want nil", *row.CloudMs)
	}
}

func TestProxy_CloudOnly_AllowListFirst(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, allowed: []string{"other-model"}, loaded: []string{"paid-model"}})
	rec := e.chat("paid-model")
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "model_not_allowed" {
		t.Fatalf("status=%d body=%s, want 403 model_not_allowed", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "cloud") {
		t.Errorf("403 body %s reveals the cloud-only policy", rec.Body.String())
	}
	if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
		t.Errorf("cloud=%d local=%d, want neither contacted", e.cloudHits.Load(), e.localHits.Load())
	}
}

func TestProxy_CloudOnly_ProviderFailure_NoLocalFallback(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}, cloudMode: "close"})
	rec := e.chat("paid-model")
	if rec.Code != http.StatusBadGateway || errCode(t, rec) != "upstream_error" {
		t.Fatalf("status=%d body=%s, want 502 upstream_error", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the model is cloud-only and was not retried locally") {
		t.Errorf("body = %s, want the not-retried-locally suffix", rec.Body.String())
	}
	if e.localHits.Load() != 0 {
		t.Errorf("local backend hit %d times after a cloud failure, want 0", e.localHits.Load())
	}
}

func TestProxy_CloudOnly_MidStreamFailure_NoLocalFallback(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}, cloudMode: "partial"})
	func() {
		defer func() {
			if p := recover(); p != nil && p != http.ErrAbortHandler {
				panic(p)
			}
		}()
		e.chat("paid-model")
	}()
	if e.localHits.Load() != 0 {
		t.Errorf("local backend hit %d times after a mid-stream cloud failure, want 0", e.localHits.Load())
	}
}

func TestProxy_CloudOnly_ManagementPathNeverCloud(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, allowMgmt: true})
	e.serve("POST", "/api/pull", `{"model":"paid-model"}`)
	if e.cloudHits.Load() != 0 {
		t.Fatalf("a management request naming a cloud-only model reached cloud %d time(s)", e.cloudHits.Load())
	}
	if e.localHits.Load() != 1 {
		t.Errorf("local hits = %d, want the management call passed through locally as before", e.localHits.Load())
	}
}

func TestProxy_CloudOnly_NonExemptPathFailsClosed(t *testing.T) {
	for _, path := range []string{"/v1/rerank", "/pooling", "/api/embed/extra"} {
		t.Run(path, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
			rec := e.serve("POST", path, `{"model":"paid-model","input":"x"}`)
			if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "cloud_only_unsupported_path" {
				t.Fatalf("status=%d body=%s, want 503 cloud_only_unsupported_path", rec.Code, rec.Body.String())
			}
			if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
				t.Errorf("cloud=%d local=%d, want neither contacted", e.cloudHits.Load(), e.localHits.Load())
			}
			row := e.lastRow(t)
			if row.RoutingReason != CloudReasonCloudOnlyPolicy || row.NodeName != "none" {
				t.Errorf("row reason=%q node=%q, want cloud_only_policy/none", row.RoutingReason, row.NodeName)
			}
		})
	}
}

func TestProxy_CloudOnly_ExemptPathsLocalRouting(t *testing.T) {
	for _, path := range []string{"/api/show", "/api/show/", "/api/ps", "/api/tags"} {
		t.Run(path, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
			rec := e.serve("POST", path, `{"model":"paid-model"}`)
			if rec.Code != http.StatusOK || e.localHits.Load() != 1 || e.cloudHits.Load() != 0 {
				t.Fatalf("status=%d local=%d cloud=%d, want today's local routing", rec.Code, e.localHits.Load(), e.cloudHits.Load())
			}
		})
		t.Run(path+" no local node", func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model"}})
			e.node.Lock()
			e.node.Healthy = false
			e.node.Unlock()
			rec := e.serve("POST", path, `{"model":"paid-model"}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want the existing 503 when no local node can answer", rec.Code)
			}
			if e.cloudHits.Load() != 0 {
				t.Fatalf("a read-only path reached the paid cloud provider %d time(s)", e.cloudHits.Load())
			}
		})
	}
}

func TestProxy_CloudOnly_ModelLookupNeverReachesCloud(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
	rec := e.serve("GET", "/v1/models/paid-model", "")
	if e.cloudHits.Load() != 0 {
		t.Fatalf("GET /v1/models/<listed> reached cloud %d time(s), status %d", e.cloudHits.Load(), rec.Code)
	}
}

func TestProxy_CloudOnly_AnthropicNonChatPath503(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/api/embed", "/api/embeddings"} {
		t.Run(path, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}, cloudProvider: "anthropic"})
			rec := e.serve("POST", path, `{"model":"paid-model","input":"x","prompt":"x"}`)
			if rec.Code != http.StatusNotImplemented || errCode(t, rec) != "unsupported_cloud_endpoint" {
				t.Fatalf("status=%d body=%s, want the existing 501", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "the model is cloud-only and was not retried locally") {
				t.Errorf("body = %s, want the not-retried-locally suffix", rec.Body.String())
			}
			if e.localHits.Load() != 0 || e.cloudHits.Load() != 0 {
				t.Errorf("local=%d cloud=%d, want neither contacted", e.localHits.Load(), e.cloudHits.Load())
			}
		})
	}
}

func TestProxy_CloudOnly_DifferentCase(t *testing.T) {
	for name, list := range map[string][]string{"exact": {"llama3"}, "prefix": {"llama*"}} {
		t.Run(name, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: list, loaded: []string{"llama3:latest"}})
			rec := e.chat("LLAMA3")
			if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
				t.Fatalf("status=%d cloud=%d local=%d, want cloud only for a different-case name", rec.Code, e.cloudHits.Load(), e.localHits.Load())
			}
		})
	}
}

func TestProxy_CloudOnly_AbsentModelNeverMatches(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model", "paid-*"}, loaded: []string{"paid-model"}})
	rec := e.serve("POST", "/v1/chat/completions", `{"messages":[]}`)
	if e.cloudHits.Load() != 0 {
		t.Fatalf("a request naming no model reached cloud (status %d)", rec.Code)
	}
}

func TestProxy_CloudOnly_MetricLabelBounded(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"Paid-Model", "paid-*"}})
	if got := e.h.metricModel("paid-model"); got != "paid-model" {
		t.Errorf("metricModel(exact entry) = %q, want the name kept", got)
	}
	// Every case and tag variant of a listed exact name shares the one
	// lower-cased label, so a client cannot mint series by varying the spelling.
	labels := map[string]struct{}{}
	for _, v := range []string{"paid-model", "PAID-MODEL", "Paid-Model", "paid-model:latest", "Paid-Model:LATEST"} {
		got := e.h.metricModel(v)
		if got != "paid-model" {
			t.Errorf("metricModel(%q) = %q, want the lower-cased list entry paid-model", v, got)
		}
		labels[got] = struct{}{}
	}
	if len(labels) != 1 {
		t.Errorf("variants produced %d labels, want 1: %v", len(labels), labels)
	}
	if got := e.h.metricModel("paid-client-invented-1"); got != "unknown" {
		t.Errorf("metricModel(prefix-only name) = %q, want unknown: client-chosen names must not mint series", got)
	}
	if got := e.h.metricModel("totally-other"); got != "unknown" {
		t.Errorf("metricModel(unlisted) = %q, want unknown", got)
	}
}

// With an empty list the request path is unchanged: the same request is served
// locally and never touches cloud.
func TestProxy_CloudOnly_EmptyListUnchanged(t *testing.T) {
	e := newCOEnv(t, coOpts{loaded: []string{"paid-model"}})
	rec := e.chat("paid-model")
	if rec.Code != http.StatusOK || e.localHits.Load() != 1 || e.cloudHits.Load() != 0 {
		t.Fatalf("status=%d local=%d cloud=%d, want served locally", rec.Code, e.localHits.Load(), e.cloudHits.Load())
	}
}

// While the stored list is unreadable the listed models are unknown, so every
// named model on a non-exempt path is refused: no local node, no cloud call.
func TestProxy_CloudOnly_UnreadableList_FailsClosed(t *testing.T) {
	e := newCOEnv(t, coOpts{loaded: []string{"any-model"}})
	e.r.SetCloudOnlyUnreadable()
	for _, path := range []string{"/v1/chat/completions", "/api/generate", "/v1/rerank"} {
		rec := e.serve("POST", path, `{"model":"any-model","messages":[]}`)
		if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "cloud_only_list_unreadable" {
			t.Fatalf("%s: status=%d body=%s, want 503 cloud_only_list_unreadable", path, rec.Code, rec.Body.String())
		}
	}
	if e.localHits.Load() != 0 || e.cloudHits.Load() != 0 {
		t.Fatalf("local=%d cloud=%d, want no request sent anywhere", e.localHits.Load(), e.cloudHits.Load())
	}
	row := e.lastRow(t)
	if row.RoutingReason != CloudReasonCloudOnlyPolicy || row.StatusCode != 503 || row.NodeName != "none" {
		t.Errorf("row reason=%q status=%d node=%q, want cloud_only_policy/503/none", row.RoutingReason, row.StatusCode, row.NodeName)
	}
	// A request naming no model is not affected.
	if rec := e.serve("POST", "/v1/chat/completions", `{"messages":[]}`); strings.Contains(rec.Body.String(), "cloud_only_list_unreadable") {
		t.Error("a request with no model name must not be refused by the unreadable-list state")
	}
}

func TestProxy_CloudOnly_UnreadableList_ExemptPathsAndRecovery(t *testing.T) {
	e := newCOEnv(t, coOpts{loaded: []string{"any-model"}})
	e.r.SetCloudOnlyUnreadable()
	if rec := e.serve("POST", "/api/show", `{"model":"any-model"}`); rec.Code != http.StatusOK || e.cloudHits.Load() != 0 {
		t.Fatalf("exempt path status=%d cloud=%d, want today's local routing", rec.Code, e.cloudHits.Load())
	}
	// Saving a valid list lifts the state.
	e.r.SetCloudOnlyModels([]string{"other"})
	if rec := e.chat("any-model"); rec.Code != http.StatusOK {
		t.Fatalf("after a valid list is set: status=%d body=%s, want 200 locally", rec.Code, rec.Body.String())
	}
	if e.r.CloudOnlyUnreadable() {
		t.Error("the unreadable state must be cleared by a valid list")
	}
}
