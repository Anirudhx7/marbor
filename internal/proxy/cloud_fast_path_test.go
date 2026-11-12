package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// fastPathEnv is one handler wired to a loopback fake cloud and a loopback
// fake local backend, with the queue knobs set the way the tests need them.
type fastPathEnv struct {
	h         *Handler
	r         *router.Router
	cloudHits atomic.Int32
	localHits atomic.Int32
	node      *router.NodeState
	st        store.Store
	localSrv  *httptest.Server
}

type fastPathOpts struct {
	runtime    string // node runtime, "" = ollama
	state      string // "down", "saturated" or "" (healthy, idle)
	timeoutMs  int
	slaMs      int
	dailyCap   float64
	localOnly  bool
	withBudget bool
}

func newFastPathEnv(t *testing.T, o fastPathOpts) *fastPathEnv {
	t.Helper()
	env := &fastPathEnv{}
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		env.cloudHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"c","choices":[],"usage":{"total_tokens":10}}`))
	}))
	t.Cleanup(cloudSrv.Close)
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		env.localHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"l","choices":[]}`))
	}))
	t.Cleanup(localSrv.Close)
	env.localSrv = localSrv

	clouds := []config.CloudProvider{{Name: "fake-openai", Provider: "openai", BaseURL: cloudSrv.URL, APIKey: "k", CostPer1KTokens: 0.002, Enabled: true}}
	r := router.New(config.RoutingConfig{MaxInFlightPerNode: 1}, []config.NodeConfig{
		{Name: "gpu-0", URL: localSrv.URL, GPUModel: "V100", Runtime: o.runtime},
	}, clouds)
	r.SetQueueMaxDepth(10)
	r.SetQueueTimeout(o.timeoutMs)
	r.SetOverflowSLA(o.slaMs)
	env.r = r
	env.node = r.Nodes()[0]
	env.node.Lock()
	env.node.LastPollAt = time.Now()
	if o.state == "down" {
		env.node.Healthy = false
	}
	env.node.Unlock()
	if o.state == "saturated" {
		r.IncrConn(env.node)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "fp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	env.st = st
	a := admin.NewServer(r, nil, config.Config{CloudBudget: config.CloudBudgetConfig{DailyUSDCap: o.dailyCap}}, st)
	t.Cleanup(a.Shutdown)
	if o.withBudget {
		a.TrackCloudCostModel("k", "openai", "gpt-4o", o.dailyCap*2, 1000)
	}
	h := NewHandler(r, a, nil)
	h.SetAuth(auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{
		{Name: "k", Key: "sk-k", RateLimit: 1000, LocalOnly: o.localOnly},
	}}))
	env.h = h
	return env
}

func (e *fastPathEnv) do() (*httptest.ResponseRecorder, time.Duration) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"model-x","messages":[]}`)))
	req = req.WithContext(context.WithValue(req.Context(), auth.KeyNameContextKey, "k"))
	rec := httptest.NewRecorder()
	start := time.Now()
	e.h.ServeHTTP(rec, req)
	return rec, time.Since(start)
}

// All nodes down with a cloud chain: the request reaches cloud in well under a
// second although the queue timeout is 5 s.
func TestProxy_HardIneligible_ReachesCloudFast(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{state: "down", timeoutMs: 5000, slaMs: 2000})
	rec, d := e.do()
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 {
		t.Fatalf("status=%d cloudHits=%d, want 200 and 1 cloud hit", rec.Code, e.cloudHits.Load())
	}
	if d > time.Second {
		t.Errorf("took %v, want under 1s (queue timeout is 5s)", d)
	}
}

// A vLLM node that does not hold the model cannot ever serve it: cloud at once.
func TestProxy_NoNodeServesModel_Fast(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{runtime: "vllm", timeoutMs: 5000, slaMs: 2000})
	rec, d := e.do()
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
		t.Fatalf("status=%d cloud=%d local=%d, want cloud only", rec.Code, e.cloudHits.Load(), e.localHits.Load())
	}
	if d > time.Second {
		t.Errorf("took %v, want under 1s", d)
	}
}

// A saturated but healthy node: wait the SLA, spill to cloud, and go back to
// local once the slot frees.
func TestProxy_Saturated_SpillsAfterSLA(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{state: "saturated", timeoutMs: 5000, slaMs: 400})
	rec, d := e.do()
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 {
		t.Fatalf("status=%d cloudHits=%d, want 200 and 1 cloud hit", rec.Code, e.cloudHits.Load())
	}
	if d < 380*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("took %v, want about the 400ms SLA, never the 5s queue timeout", d)
	}
	e.r.DecrConn(e.node)
	rec, _ = e.do()
	if rec.Code != http.StatusOK || e.localHits.Load() != 1 || e.cloudHits.Load() != 1 {
		t.Fatalf("after the slot freed: status=%d local=%d cloud=%d, want served locally", rec.Code, e.localHits.Load(), e.cloudHits.Load())
	}
}

// A local_only key never reaches cloud, for the hard classes and for a full
// node alike, and keeps the full queue wait because cloud is not usable.
func TestProxy_LocalOnly_Never_Cloud(t *testing.T) {
	for name, o := range map[string]fastPathOpts{
		"all_nodes_unavailable": {state: "down"},
		"no_node_serves_model":  {runtime: "vllm"},
		"saturated":             {state: "saturated"},
	} {
		t.Run(name, func(t *testing.T) {
			o.timeoutMs, o.slaMs, o.localOnly = 300, 50, true
			e := newFastPathEnv(t, o)
			rec, d := e.do()
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "local_only_blocked") {
				t.Fatalf("status=%d body=%s, want 503 local_only_blocked", rec.Code, rec.Body.String())
			}
			if e.cloudHits.Load() != 0 {
				t.Errorf("cloud was hit %d times for a local_only key", e.cloudHits.Load())
			}
			if d < 250*time.Millisecond {
				t.Errorf("took %v, want the full 300ms queue wait (cloud not usable, SLA must not apply)", d)
			}
		})
	}
}

// A local_only key turned away by the queue stage carries Retry-After equal to
// the queue timeout rounded up, like the plain no-nodes 503.
func TestProxy_LocalOnlyBlocked_RetryAfter(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{state: "saturated", timeoutMs: 1100, slaMs: 50, localOnly: true})
	rec, _ := e.do()
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "local_only_blocked") {
		t.Fatalf("status=%d body=%s, want 503 local_only_blocked", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (1100ms queue timeout rounded up)", got)
	}
}

// With the cloud budget exhausted, cloud is not usable: the request keeps the
// full wait and ends in the budget 503, never reaching the provider.
func TestProxy_BudgetExceeded_KeepsFullWait(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{state: "saturated", timeoutMs: 400, slaMs: 50, dailyCap: 1, withBudget: true})
	rec, d := e.do()
	if rec.Code != http.StatusServiceUnavailable || e.cloudHits.Load() != 0 {
		t.Fatalf("status=%d cloudHits=%d, want 503 and no cloud hit", rec.Code, e.cloudHits.Load())
	}
	if d < 350*time.Millisecond {
		t.Errorf("took %v, want the full 400ms queue wait, not the 50ms SLA", d)
	}
}
