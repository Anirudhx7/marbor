package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestNodesForEntry(t *testing.T) {
	nodes := []*NodeState{
		{Name: "gpu-1"},
		{Name: "gpu-2"},
		{Name: "gpu-3"},
	}

	// empty allow list = all nodes
	got := nodesForEntry(nodes, nil)
	if len(got) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(got))
	}

	// specific subset
	got = nodesForEntry(nodes, []string{"gpu-1", "gpu-3"})
	if len(got) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(got))
	}
	if got[0].Name != "gpu-1" || got[1].Name != "gpu-3" {
		t.Errorf("unexpected nodes: %v", got)
	}

	// unknown name = no match
	got = nodesForEntry(nodes, []string{"gpu-99"})
	if len(got) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(got))
	}
}

func TestPingNodeUnhealthySkipped(t *testing.T) {
	r := &Router{client: &http.Client{Timeout: 1 * time.Second}}
	n := &NodeState{Name: "dead", URL: "http://127.0.0.1:19999", Healthy: false}
	err := r.pingNode(context.Background(), n, "llama3.2", "10m")
	if err == nil {
		t.Fatal("expected error for unhealthy node, got nil")
	}
}

func TestPingNodeSuccess(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	router := &Router{client: &http.Client{Timeout: 5 * time.Second}}
	n := &NodeState{Name: "test", URL: srv.URL, Healthy: true}
	err := router.pingNode(context.Background(), n, "llama3.2", "10m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
}

func TestPingWarmupModelsDisabled(t *testing.T) {
	// disabled warmup = no panics, no HTTP calls
	r := &Router{
		client:    &http.Client{Timeout: time.Second},
		warmupCfg: config.WarmupConfig{Enabled: false},
	}
	r.pingWarmupModels(context.Background()) // should return immediately
}

func TestPingWarmupModelsAllNodes(t *testing.T) {
	calls := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Host
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		client: &http.Client{Timeout: 5 * time.Second},
		nodes:  []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		warmupCfg: config.WarmupConfig{
			Enabled:   true,
			KeepAlive: "5m",
			Models:    []config.WarmupEntry{{Model: "llama3.2"}},
		},
	}
	r.pingWarmupModels(context.Background())

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("warmup ping never fired")
	}
}

// TestPerNodeRuntimeWarmupPings verifies that per-node warmup (toggled at
// runtime via the admin API, persisted in the KV store) fires pings even when
// config-file warmup is disabled.
func TestPerNodeRuntimeWarmupPings(t *testing.T) {
	calls := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		client:     &http.Client{Timeout: 5 * time.Second},
		nodes:      []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		nodeWarmup: map[string]NodeWarmup{"n1": {Enabled: true, Models: []string{"llama3.2"}}},
		// config-file warmup intentionally left disabled.
	}
	r.pingWarmupModels(context.Background())
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("per-node runtime warmup ping never fired")
	}
}

// TestPingWarmupModelsSequentialPerNode verifies that when multiple models are
// warmed on the SAME node, their pings are never in flight at the same time.
// Firing them concurrently races ensureHeadroom's headroom check (both calls
// would see the identical pre-warmup LoadedModels snapshot) and hands the
// real runtime two competing cold loads to arbitrate itself - which is how a
// node ends up with only one of the two requested models actually resident.
func TestPingWarmupModelsSequentialPerNode(t *testing.T) {
	var inFlight int32
	var maxInFlight int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond) // simulate a slow cold load
		atomic.AddInt32(&inFlight, -1)
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes: []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		nodeWarmup: map[string]NodeWarmup{
			"n1": {Enabled: true, Models: []string{"llama3", "mistral"}},
		},
	}
	r.pingWarmupModels(context.Background())
	time.Sleep(400 * time.Millisecond) // both pings should have completed by now

	if got := atomic.LoadInt32(&maxInFlight); got > 1 {
		t.Errorf("models on the same node were pinged concurrently (max in-flight = %d), want sequential (1)", got)
	}
}

// TestWarmupBothModelsStayResidentOnNodeWithHeadroom reproduces the reported
// bug end to end: a node with two distinct models selected for warmup, and
// enough combined VRAM for both, should end up with BOTH resident. The fake
// node below models a runtime that can only complete one concurrent cold
// load at a time (an observed real-world characteristic): if a second load
// request arrives while an earlier one on a different model is still being
// processed, the earlier one is aborted rather than becoming resident. That
// is the actual failure mode this bug produces. Sequential per-node dispatch
// (the fix) never presents the node with two overlapping loads, so both
// models end up resident.
func TestWarmupBothModelsStayResidentOnNodeWithHeadroom(t *testing.T) {
	var mu sync.Mutex
	inFlightModel := ""
	aborted := map[string]bool{}
	resident := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)

		mu.Lock()
		if inFlightModel != "" && inFlightModel != body.Model {
			// A different model started loading while this node already had
			// one in flight: a real GPU can't hold two uncommitted cold
			// loads' worth of VRAM at once, so the earlier one is aborted.
			aborted[inFlightModel] = true
		}
		inFlightModel = body.Model
		mu.Unlock()

		time.Sleep(50 * time.Millisecond) // simulate a slow cold load

		mu.Lock()
		if inFlightModel == body.Model {
			inFlightModel = ""
		}
		if !aborted[body.Model] {
			resident[body.Model] = true
		}
		mu.Unlock()

		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes: []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		nodeWarmup: map[string]NodeWarmup{
			"n1": {Enabled: true, Models: []string{"llama3", "mistral"}},
		},
	}
	r.pingWarmupModels(context.Background())
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !resident["llama3"] || !resident["mistral"] {
		t.Fatalf("expected both models to stay resident after warmup, got %v", resident)
	}
}

// TestPingNodeRequestBody verifies pingNode sends the correct JSON body to
// /api/generate: model name, keep_alive string, and stream:false. This is the
// critical correctness test — a malformed body would silently fail to keep the
// model warm even though the HTTP call succeeded.
func TestPingNodeRequestBody(t *testing.T) {
	type reqBody struct {
		Model     string `json:"model"`
		KeepAlive string `json:"keep_alive"`
		Stream    bool   `json:"stream"`
	}
	received := make(chan reqBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b reqBody
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Errorf("unmarshal body: %v", err)
		}
		received <- b
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	router := &Router{client: &http.Client{Timeout: 5 * time.Second}}
	n := &NodeState{Name: "test", URL: srv.URL, Healthy: true}
	if err := router.pingNode(context.Background(), n, "llama3.2:8b", "15m"); err != nil {
		t.Fatalf("pingNode error: %v", err)
	}
	select {
	case b := <-received:
		if b.Model != "llama3.2:8b" {
			t.Errorf("model = %q, want %q", b.Model, "llama3.2:8b")
		}
		if b.KeepAlive != "15m" {
			t.Errorf("keep_alive = %q, want %q", b.KeepAlive, "15m")
		}
		if b.Stream {
			t.Error("stream should be false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
	}
}

// TestPingNodeDrainBody verifies that a keep_alive of "0" (drain/unload) is
// forwarded verbatim — sending any other value would prevent the model from
// being evicted from VRAM during a scheduled drain.
func TestPingNodeDrainBody(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		ka, _ := b["keep_alive"].(string)
		received <- ka
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	router := &Router{client: &http.Client{Timeout: 5 * time.Second}}
	n := &NodeState{Name: "test", URL: srv.URL, Healthy: true}
	if err := router.pingNode(context.Background(), n, "llama3.2:8b", "0"); err != nil {
		t.Fatalf("pingNode drain error: %v", err)
	}
	select {
	case ka := <-received:
		if ka != "0" {
			t.Errorf("keep_alive = %q, want \"0\" for drain", ka)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
	}
}

// TestEffectiveKeepAlive verifies the keep_alive is bumped past the interval so
// models never unload between pings, and preserved when already long enough.
func TestEffectiveKeepAlive(t *testing.T) {
	if got := effectiveKeepAlive("1m", 10*time.Minute); got == "1m" {
		t.Errorf("effectiveKeepAlive(1m, 10m) = %q, want a value >= interval", got)
	}
	if got := effectiveKeepAlive("30m", 10*time.Minute); got != "30m" {
		t.Errorf("effectiveKeepAlive(30m, 10m) = %q, want 30m (preserved)", got)
	}
	if got := effectiveKeepAlive("", 5*time.Minute); got == "" {
		t.Error("effectiveKeepAlive with empty config returned empty")
	}
}
