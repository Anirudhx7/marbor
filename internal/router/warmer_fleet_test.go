package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// recordingNode is a fake Ollama backend that records every warm ping and
// every unload (keep_alive 0) by model name, and serves /api/tags from sizes.
type recordingNode struct {
	srv      *httptest.Server
	mu       sync.Mutex
	pings    []string
	unloads  []string
	requests int32
	fail     atomic.Bool
}

func newRecordingNode(t *testing.T, sizes map[string]int64) *recordingNode {
	t.Helper()
	rn := &recordingNode{}
	rn.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rn.requests, 1)
		if r.URL.Path == "/api/tags" {
			type tag struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}
			var out struct {
				Models []tag `json:"models"`
			}
			for name, size := range sizes {
				out.Models = append(out.Models, tag{Name: name, Size: size})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model     string `json:"model"`
			KeepAlive any    `json:"keep_alive"`
		}
		_ = json.Unmarshal(b, &body)
		rn.mu.Lock()
		if ka, ok := body.KeepAlive.(float64); ok && ka == 0 {
			rn.unloads = append(rn.unloads, body.Model)
		} else {
			rn.pings = append(rn.pings, body.Model)
		}
		rn.mu.Unlock()
		if rn.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"done":true}`))
	}))
	t.Cleanup(rn.srv.Close)
	return rn
}

func (rn *recordingNode) pinged() []string {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return append([]string(nil), rn.pings...)
}

func (rn *recordingNode) unloaded() []string {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return append([]string(nil), rn.unloads...)
}

// waitWarmupIdle blocks until no warmup goroutine holds nodeName's slot.
func waitWarmupIdle(t *testing.T, r *Router, nodeName string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.warmupInProgressMu.Lock()
		busy := r.warmupInProgress[nodeName]
		r.warmupInProgressMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("warmup for %s never finished", nodeName)
}

func nodeWarnings(n *NodeState) map[string]string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make(map[string]string, len(n.WarmupWarnings))
	for k, v := range n.WarmupWarnings {
		out[k] = v
	}
	return out
}

// TestPingWarmupModels_WorkerSkippedWithSourceSpecificWarnings: a replica
// worker is never warmed, whatever configured it. A global entry with no node
// list still warms the head and is skipped on the worker silently; pairs that
// explicitly named the worker get a warning whose remediation names the
// source(s) that did, and each part clears once its own source is gone.
func TestPingWarmupModels_WorkerSkippedWithSourceSpecificWarnings(t *testing.T) {
	head := newRecordingNode(t, nil)
	worker := newRecordingNode(t, nil)
	decl := &store.ReplicaPeers{Members: []string{"head", "worker"}, Head: "head"}
	hn := &NodeState{Name: "head", URL: head.srv.URL, Healthy: true, ReplicaPeers: decl}
	wn := &NodeState{Name: "worker", URL: worker.srv.URL, Healthy: true, ReplicaPeers: decl}
	r := &Router{
		nodes: []*NodeState{hn, wn},
		warmupCfg: config.WarmupConfig{
			Enabled:   true,
			KeepAlive: "5m",
			Models: []config.WarmupEntry{
				{Model: "global"},
				{Model: "cfg-named", Nodes: []string{"worker"}},
				{Model: "both", Nodes: []string{"worker"}},
			},
		},
		nodeWarmup: map[string]NodeWarmup{"worker": {Enabled: true, Models: []string{"node-named", "both"}}},
	}

	r.pingWarmupModels(context.Background())
	waitWarmupIdle(t, r, "head")

	if got := worker.pinged(); len(got) != 0 {
		t.Fatalf("worker was pinged %v; a replica worker must never be a warmup target", got)
	}
	if got := head.pinged(); len(got) != 1 || got[0] != "global" {
		t.Fatalf("head pings = %v, want [global] (a global entry still warms the head)", got)
	}
	w := nodeWarnings(wn)
	if _, ok := w["global"]; ok {
		t.Errorf("globally-expanded pair on a worker must be skipped silently, got warning %q", w["global"])
	}
	nodeClear := "marbor nodes warmup set worker --enabled=false"
	cfgClear := "Remove worker from the node list of the global warmup entry"
	if m := w["node-named"]; !strings.Contains(m, "head head") || !strings.Contains(m, nodeClear) || strings.Contains(m, cfgClear) {
		t.Errorf("node-named warning = %q, want only the per-node remediation", m)
	}
	if m := w["cfg-named"]; !strings.Contains(m, cfgClear) || strings.Contains(m, nodeClear) {
		t.Errorf("cfg-named warning = %q, want only the global-settings remediation", m)
	}
	if m := w["both"]; !strings.Contains(m, cfgClear) || !strings.Contains(m, nodeClear) {
		t.Errorf("both warning = %q, want both remediations", m)
	}
	if len(nodeWarnings(hn)) != 0 {
		t.Errorf("head should carry no warnings, got %v", nodeWarnings(hn))
	}

	// Clear the worker's per-node config (what the clear path sends).
	r.SetNodeWarmup("worker", false, nil)
	r.pingWarmupModels(context.Background())
	waitWarmupIdle(t, r, "head")
	w = nodeWarnings(wn)
	if _, ok := w["node-named"]; ok {
		t.Errorf("node-named warning should clear once the per-node config is gone, got %q", w["node-named"])
	}
	if m := w["both"]; !strings.Contains(m, cfgClear) || strings.Contains(m, nodeClear) {
		t.Errorf("both warning after per-node clear = %q, want only the global-settings part", m)
	}
	if _, ok := w["cfg-named"]; !ok {
		t.Error("cfg-named warning must persist until the global entry is fixed")
	}

	// Fix the global entries too: every warning goes.
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, KeepAlive: "5m", Models: []config.WarmupEntry{{Model: "global"}}})
	r.pingWarmupModels(context.Background())
	waitWarmupIdle(t, r, "head")
	if w := nodeWarnings(wn); len(w) != 0 {
		t.Errorf("worker warnings after fixing every source = %v, want none", w)
	}
}

// TestPingWarmupModels_UnresolvedSkippedWithWarning: an unresolved node is
// never warmed and every one of its pairs is annotated, whatever the source.
func TestPingWarmupModels_UnresolvedSkippedWithWarning(t *testing.T) {
	a := newRecordingNode(t, nil)
	b := newRecordingNode(t, nil)
	an := &NodeState{Name: "a", URL: a.srv.URL, Healthy: true, ReplicaPeers: &store.ReplicaPeers{Members: []string{"a", "b"}, Head: "a"}}
	bn := &NodeState{Name: "b", URL: b.srv.URL, Healthy: true}
	r := &Router{
		nodes:      []*NodeState{an, bn},
		warmupCfg:  config.WarmupConfig{Enabled: true, KeepAlive: "5m", Models: []config.WarmupEntry{{Model: "global"}}},
		nodeWarmup: map[string]NodeWarmup{"a": {Enabled: true, Models: []string{"m"}}},
	}
	r.pingWarmupModels(context.Background())
	time.Sleep(100 * time.Millisecond)
	if len(a.pinged())+len(b.pinged()) != 0 {
		t.Fatalf("unresolved nodes were pinged: a=%v b=%v", a.pinged(), b.pinged())
	}
	for _, n := range []*NodeState{an, bn} {
		w := nodeWarnings(n)
		if !strings.Contains(w["global"], "unresolved replica declaration") {
			t.Errorf("%s: global warning = %q", n.Name, w["global"])
		}
	}
	if !strings.Contains(nodeWarnings(an)["m"], "reconcile replica_peers") {
		t.Errorf("a: m warning = %q", nodeWarnings(an)["m"])
	}
}

// TestPingWarmupModels_DigestDriftWarnsAndStillWarms: drift is surfaced as a
// warning, the model is still pinged, a successful ping does not erase the
// warning, a failed ping records an error without touching it, and it clears
// on the first cycle the digests agree again. A bare configured name is
// checked against its ":latest" resident entry.
func TestPingWarmupModels_DigestDriftWarnsAndStillWarms(t *testing.T) {
	rn := newRecordingNode(t, nil)
	n := &NodeState{Name: "n1", URL: rn.srv.URL, Healthy: true,
		LoadedModels: []ModelInfo{{Name: "llama3:latest", Digest: "drifted"}}}
	r := &Router{
		nodes:        []*NodeState{n},
		nodeWarmup:   map[string]NodeWarmup{"n1": {Enabled: true, Models: []string{"llama3", "cold"}}},
		modelDigests: map[string]string{"llama3:latest": "reference"},
	}

	r.pingWarmupModels(context.Background())
	waitWarmupIdle(t, r, "n1")
	if got := rn.pinged(); len(got) != 2 {
		t.Fatalf("pings = %v, want both models still pinged", got)
	}
	w := nodeWarnings(n)
	if !strings.Contains(w["llama3"], "digest drift") {
		t.Fatalf("llama3 warning = %q, want digest drift after a successful ping", w["llama3"])
	}
	if _, ok := w["cold"]; ok {
		t.Error("a model that is not resident must not be flagged")
	}
	n.mu.RLock()
	errs := len(n.WarmupErrors)
	n.mu.RUnlock()
	if errs != 0 {
		t.Errorf("drift must not be recorded as a warmup error, got %v", n.WarmupErrors)
	}

	// A failing ping records an error and leaves the warning alone.
	rn.fail.Store(true)
	r.pingWarmupModels(context.Background())
	waitWarmupIdle(t, r, "n1")
	n.mu.RLock()
	gotErr := n.WarmupErrors["llama3"]
	n.mu.RUnlock()
	if gotErr == "" {
		t.Error("failed ping should populate WarmupErrors")
	}
	if !strings.Contains(nodeWarnings(n)["llama3"], "digest drift") {
		t.Error("failed ping must not touch the drift warning")
	}

	// Digests converge: the warning clears on the next cycle.
	rn.fail.Store(false)
	n.mu.Lock()
	n.LoadedModels = []ModelInfo{{Name: "llama3:latest", Digest: "reference"}}
	n.mu.Unlock()
	r.pingWarmupModels(context.Background())
	waitWarmupIdle(t, r, "n1")
	if w := nodeWarnings(n); len(w) != 0 {
		t.Errorf("warnings after digests agree = %v, want none", w)
	}
}

// TestPingWarmupModels_EmptyConfigClearsWarnings: with nothing left to warm
// the cycle still resets every node's warnings.
func TestPingWarmupModels_EmptyConfigClearsWarnings(t *testing.T) {
	n := &NodeState{Name: "n1", Healthy: true, WarmupWarnings: map[string]string{"m": "stale"}}
	r := &Router{nodes: []*NodeState{n}}
	r.pingWarmupModels(context.Background())
	if w := nodeWarnings(n); len(w) != 0 {
		t.Errorf("warnings = %v, want cleared", w)
	}
}

// TestWarmupSkipsUnhealthyNodeBeforeHeadroom: an unhealthy (not draining)
// node gets no request at all - not a size lookup, not an eviction, not a
// ping - from either the keep-warm pinger or a scheduled warmup.
func TestWarmupSkipsUnhealthyNodeBeforeHeadroom(t *testing.T) {
	rn := newRecordingNode(t, map[string]int64{"new": 40 * mib})
	n := &NodeState{Name: "n1", URL: rn.srv.URL, Healthy: false, VRAMTotalMB: 50,
		LoadedModels: []ModelInfo{{Name: "old", SizeVRAM: 40 * mib}}}
	r := &Router{
		client:     &http.Client{Timeout: 5 * time.Second},
		nodes:      []*NodeState{n},
		nodeWarmup: map[string]NodeWarmup{"n1": {Enabled: true, Models: []string{"new"}}},
		lastUsed:   map[string]time.Time{},
		tagsCache:  map[string]*TagsCache{},
	}
	r.pingWarmupModels(context.Background())
	if err := r.WarmModels(context.Background(), "n1", []string{"new"}); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("WarmModels on unhealthy node: err = %v, want unhealthy", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&rn.requests); got != 0 {
		t.Errorf("unhealthy node received %d request(s) (unloads %v, pings %v); want 0", got, rn.unloaded(), rn.pinged())
	}
}

// TestWarmModels_ReplicaTopology: a scheduled warmup naming a worker warms
// its head, an unresolved node is an error, and digest drift neither blocks
// the warmup nor fails the run.
func TestWarmModels_ReplicaTopology(t *testing.T) {
	head := newRecordingNode(t, nil)
	worker := newRecordingNode(t, nil)
	decl := &store.ReplicaPeers{Members: []string{"head", "worker"}, Head: "head"}
	hn := &NodeState{Name: "head", URL: head.srv.URL, Healthy: true, ReplicaPeers: decl,
		LoadedModels: []ModelInfo{{Name: "llama3:latest", Digest: "drifted"}}}
	wn := &NodeState{Name: "worker", URL: worker.srv.URL, Healthy: true, ReplicaPeers: decl}
	un := &NodeState{Name: "lonely", URL: worker.srv.URL, Healthy: true,
		ReplicaPeers: &store.ReplicaPeers{Members: []string{"lonely", "ghost"}, Head: "lonely"}}
	r := &Router{
		nodes:        []*NodeState{hn, wn, un},
		modelDigests: map[string]string{"llama3:latest": "reference"},
	}

	if err := r.WarmModels(context.Background(), "worker", []string{"llama3"}); err != nil {
		t.Fatalf("WarmModels(worker) = %v, want nil (drift is warn-only)", err)
	}
	if got := head.pinged(); len(got) != 1 || got[0] != "llama3" {
		t.Errorf("head pings = %v, want [llama3]", got)
	}
	if got := worker.pinged(); len(got) != 0 {
		t.Errorf("worker pings = %v, want none", got)
	}
	if err := r.WarmModels(context.Background(), "lonely", []string{"llama3"}); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("WarmModels(unresolved) = %v, want unresolved error", err)
	}
}

// TestRunPredictionCycle_ExcludesWorkersUnresolvedAndNonOllama: predictive
// only ever targets standalone and head nodes on a runtime that can warm.
func TestRunPredictionCycle_ExcludesWorkersUnresolvedAndNonOllama(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "standalone", URL: "http://standalone:11434", VRAMTotalMB: 16384},
		{Name: "head", URL: "http://head:11434", VRAMTotalMB: 16384},
		{Name: "worker", URL: "http://worker:11434", VRAMTotalMB: 16384},
		{Name: "lonely", URL: "http://lonely:11434", VRAMTotalMB: 16384},
		{Name: "vllm", URL: "http://vllm:8000", VRAMTotalMB: 16384, Runtime: "vllm"},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
	decl := &store.ReplicaPeers{Members: []string{"head", "worker"}, Head: "head"}
	for _, n := range r.nodes {
		n.LoadedModels = []ModelInfo{{Name: "model-w", SizeVRAM: 2000 * mib}}
		n.VRAMUsedMB = 2000
		switch n.Name {
		case "head", "worker":
			n.ReplicaPeers = decl
		case "lonely":
			n.ReplicaPeers = &store.ReplicaPeers{Members: []string{"lonely", "ghost"}, Head: "lonely"}
		}
	}
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("model-w", now)
	r.RecordTransition("model-x", now)
	r.RunPredictionCycle(context.Background(), now)

	got := map[string]bool{}
	for _, d := range r.RecentPredictiveDecisions() {
		got[d.Node] = true
	}
	for _, name := range []string{"worker", "lonely", "vllm"} {
		if got[name] {
			t.Errorf("predictive considered %q; workers, unresolved and non-Ollama nodes must be excluded", name)
		}
	}
	for _, name := range []string{"standalone", "head"} {
		if !got[name] {
			t.Errorf("predictive skipped eligible node %q (decisions: %+v)", name, r.RecentPredictiveDecisions())
		}
	}
}

// TestEvictForHeadroom_SessionAffinityIsSoftProtection: a model a live
// sticky session is using on this node is passed over while another
// candidate exists (a bare session model name matches the ":latest" loaded
// entry), is still evicted as a last resort, and an expired session protects
// nothing.
func TestEvictForHeadroom_SessionAffinityIsSoftProtection(t *testing.T) {
	setup := func(t *testing.T, loaded []ModelInfo, lastSeen time.Time) (*Router, *recordingNode) {
		rn := newRecordingNode(t, nil)
		r := &Router{
			nodes:       []*NodeState{{Name: "n1", URL: rn.srv.URL, Healthy: true, VRAMTotalMB: 100, LoadedModels: loaded}},
			lastUsed:    map[string]time.Time{},
			pinned:      map[string]map[string]bool{},
			affinity:    map[string]*affinityEntry{},
			affinityTTL: time.Hour,
		}
		r.lastUsed[modelKey("n1", "llama3:latest")] = time.Now().Add(-2 * time.Hour) // coldest
		r.lastUsed[modelKey("n1", "other")] = time.Now().Add(-time.Minute)
		e := &affinityEntry{nodeURL: rn.srv.URL, model: "llama3"}
		e.lastSeen.Store(lastSeen.UnixNano())
		r.affinity["s1"] = e
		return r, rn
	}

	t.Run("prefers the unprotected candidate", func(t *testing.T) {
		r, rn := setup(t, []ModelInfo{{Name: "llama3:latest", SizeVRAM: 45 * mib}, {Name: "other", SizeVRAM: 45 * mib}}, time.Now())
		r.EvictForHeadroom(context.Background(), "n1", "new", 40*mib)
		if got := rn.unloaded(); len(got) != 1 || got[0] != "other" {
			t.Errorf("unloads = %v, want [other] (llama3 has a live session)", got)
		}
	})
	t.Run("last resort still evicts it", func(t *testing.T) {
		r, rn := setup(t, []ModelInfo{{Name: "llama3:latest", SizeVRAM: 90 * mib}}, time.Now())
		r.EvictForHeadroom(context.Background(), "n1", "new", 40*mib)
		if got := rn.unloaded(); len(got) != 1 || got[0] != "llama3:latest" {
			t.Errorf("unloads = %v, want [llama3:latest] as the last resort", got)
		}
	})
	t.Run("expired session protects nothing", func(t *testing.T) {
		r, rn := setup(t, []ModelInfo{{Name: "llama3:latest", SizeVRAM: 45 * mib}, {Name: "other", SizeVRAM: 45 * mib}}, time.Now().Add(-2*time.Hour))
		r.EvictForHeadroom(context.Background(), "n1", "new", 40*mib)
		if got := rn.unloaded(); len(got) != 1 || got[0] != "llama3:latest" {
			t.Errorf("unloads = %v, want plain LRU [llama3:latest]", got)
		}
	})
}

// TestStampAffinityModel: a model change swaps in a new entry for the same
// node with lastSeen preserved, and never touches an entry that moved to a
// different node or was deleted.
func TestStampAffinityModel(t *testing.T) {
	r := &Router{affinity: map[string]*affinityEntry{}}
	old := &affinityEntry{nodeURL: "http://a", model: "m1"}
	old.lastSeen.Store(42)
	r.affinity["s"] = old

	r.stampAffinityModel("s", "http://a", "m2")
	got := r.affinity["s"]
	if got == old || got.model != "m2" || got.nodeURL != "http://a" || got.lastSeen.Load() != 42 {
		t.Fatalf("after stamp: %+v (same pointer: %v)", got, got == old)
	}
	if old.model != "m1" {
		t.Error("the published entry must never be mutated in place")
	}

	r.stampAffinityModel("s", "http://b", "m3")
	if r.affinity["s"].model != "m2" {
		t.Error("stamp for a different node must not change the entry")
	}
	r.stampAffinityModel("missing", "http://a", "m1")
	if _, ok := r.affinity["missing"]; ok {
		t.Error("stamp must never create an entry")
	}
}

// TestConcurrentScheduledAndPredictiveWarmupEviction is the regression
// check for a scheduled warmup and a predictive prewarm racing on the same
// node for different models, both needing room from the same evictable
// pool. Correctness here means the race never does worse than running the
// two back to back would: no model is unloaded twice, and no more models are
// evicted than the sequential run evicts.
func TestConcurrentScheduledAndPredictiveWarmupEviction(t *testing.T) {
	run := func(t *testing.T, concurrent bool) []string {
		rn := newRecordingNode(t, map[string]int64{"sched-model": 25 * mib, "pred-model": 25 * mib})
		r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: rn.srv.URL}}, nil)
		n := r.nodes[0]
		n.Healthy = true
		n.VRAMTotalMB = 100
		n.LoadedModels = []ModelInfo{
			{Name: "old1", SizeVRAM: 30 * mib},
			{Name: "old2", SizeVRAM: 30 * mib},
			{Name: "old3", SizeVRAM: 30 * mib},
		}
		base := time.Now().Add(-time.Hour)
		for i, m := range []string{"old1", "old2", "old3"} {
			r.lastUsed[modelKey("n1", m)] = base.Add(time.Duration(i) * time.Minute)
		}
		predictive := func() { r.prewarmPredicted(context.Background(), n, "pred-model", "5m") }
		scheduled := func() { _ = r.WarmModels(context.Background(), "n1", []string{"sched-model"}) }
		if concurrent {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for _, f := range []func(){scheduled, predictive} {
				wg.Add(1)
				go func(f func()) {
					defer wg.Done()
					<-start
					f()
				}(f)
			}
			close(start)
			wg.Wait()
		} else {
			scheduled()
			predictive()
		}
		return rn.unloaded()
	}

	sequential := run(t, false)
	for i := 0; i < 20; i++ {
		got := run(t, true)
		seen := map[string]bool{}
		for _, m := range got {
			if seen[m] {
				t.Fatalf("iteration %d: %q unloaded twice (unloads %v)", i, m, got)
			}
			seen[m] = true
		}
		if len(got) > len(sequential) {
			t.Fatalf("iteration %d: concurrent run evicted %v, more than the sequential run's %v", i, got, sequential)
		}
	}
}
