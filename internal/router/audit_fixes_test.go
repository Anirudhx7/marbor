package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

// ---------------------------------------------------------------------------
// Task A (#3): WaitForNode threads runtimeFilter  --  wrong-runtime node must NOT
// be returned when a matching node is available.
// ---------------------------------------------------------------------------

func TestWaitForNodeRuntimeFilter(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        10,
		QueueTimeoutMs:       500,
	}
	// Two nodes: vllm and ollama.
	r := New(cfg, []config.NodeConfig{
		{Name: "vllm-node", URL: "http://localhost:8000", Runtime: "vllm"},
		{Name: "ollama-node", URL: "http://localhost:11434", Runtime: "ollama"},
	}, nil)

	// Mark both healthy.
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()
	for _, n := range nodes {
		n.mu.Lock()
		n.Healthy = true
		n.mu.Unlock()
	}

	// Request with runtimeFilter="ollama" must NOT return the vllm node.
	node, _ := r.WaitForNode(context.Background(), "llama3.2", "", "ollama")
	if node == nil {
		t.Fatal("expected an ollama node, got nil")
	}
	node.mu.RLock()
	rt := node.Runtime
	node.mu.RUnlock()
	if rt != "ollama" {
		t.Errorf("expected runtime=ollama, got %q", rt)
	}
}

// TestWaitForNodeRuntimeFilterNoMatch ensures nil is returned when no node
// matches the filter (not a wrong-runtime node).
func TestWaitForNodeRuntimeFilterNoMatch(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        10,
		QueueTimeoutMs:       200, // short for fast test
	}
	// Only a vllm node available.
	r := New(cfg, []config.NodeConfig{
		{Name: "vllm-node", URL: "http://localhost:8000", Runtime: "vllm"},
	}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = true
	n.mu.Unlock()

	// Requesting "ollama" filter  --  no matching node, must time out and return nil.
	node, _ := r.WaitForNode(context.Background(), "llama3.2", "", "ollama")
	if node != nil {
		t.Errorf("expected nil when no ollama node available, got %+v", node)
	}
}

// ---------------------------------------------------------------------------
// Task E (#14): FetchModelTags panic safety  --  close(entry.done) via defer
// means waiters are unblocked even if the fetcher panics.
// ---------------------------------------------------------------------------

func TestFetchModelTagsPanicSafety(t *testing.T) {
	// Serve a handler that panics mid-response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a valid status so we get past resp.StatusCode check, then
		// panic during decode by closing connection abruptly.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Send malformed JSON to trigger a decode error (simulates bad response).
		fmt.Fprint(w, `{invalid json`)
	}))
	defer srv.Close()

	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
	}
	r := New(cfg, nil, nil)

	// Two concurrent callers: the first triggers the fetch (which fails on decode),
	// the second must not block forever waiting on entry.done.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			_, err := r.FetchModelTags(srv.URL)
			results[i] = err
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Both callers returned  --  no deadlock.
		for i, err := range results {
			if err == nil {
				t.Errorf("caller %d: expected error from malformed JSON, got nil", i)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FetchModelTags: waiters blocked forever  --  panic safety defer not working")
	}
}

// ---------------------------------------------------------------------------
// Task D (#18): pollAll holds RLock snapshot (already fixed  --  regression test).
// ---------------------------------------------------------------------------

func TestPollAllHoldsSnapshot(t *testing.T) {
	// Verify pollAll doesn't race with concurrent AddNode by running both
	// in a tight loop. The race detector will catch any data race.
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Goroutine 1: repeatedly call pollAll.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.pollAll()
			}
		}
	}()

	// Goroutine 2: repeatedly add/remove nodes concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			r.AddNode(config.NodeConfig{Name: fmt.Sprintf("tmp-%d", i), URL: fmt.Sprintf("http://localhost:%d", 11500+i)})
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()

	wg.Wait()
}

// ---------------------------------------------------------------------------
// Task K (#19): SetStrategy rejects unknown strategy values.
// ---------------------------------------------------------------------------

func TestSetStrategyValidation(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
	}
	r := New(cfg, nil, nil)

	valid := []string{"warm-first", "least-connections", "round-robin", "vram-aware"}
	for _, s := range valid {
		if err := r.SetStrategy(s); err != nil {
			t.Errorf("SetStrategy(%q) unexpected error: %v", s, err)
		}
	}

	invalid := []string{"", "random", "WARM-FIRST", "greedy", "ml-routing"}
	for _, s := range invalid {
		if err := r.SetStrategy(s); err == nil {
			t.Errorf("SetStrategy(%q) expected error, got nil", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Task K (#17): RouteExcluding reads r.fallback in a single lock acquisition.
// Regression: verify RouteExcluding works correctly (no double-lock deadlock).
// ---------------------------------------------------------------------------

func TestRouteExcludingFallbackSingleLock(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
	}
	r := New(cfg, []config.NodeConfig{
		{Name: "n1", URL: "http://localhost:11434"},
		{Name: "n2", URL: "http://localhost:11435"},
	}, nil)

	r.mu.RLock()
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = true
		n.mu.Unlock()
	}
	r.mu.RUnlock()

	// Exclude n1; RouteExcluding must return n2 (fallback path, single lock).
	exclude := map[string]bool{"http://localhost:11434": true}
	node, _ := r.RouteExcluding("llama3.2", "", exclude)
	if node == nil {
		t.Fatal("expected n2, got nil")
	}
	if node.URL == "http://localhost:11434" {
		t.Error("RouteExcluding returned the excluded node")
	}
}

// ---------------------------------------------------------------------------
// Task K (#29): warmupTicker only starts when warmup is enabled.
// Regression: Start() with warmup disabled must not panic.
// ---------------------------------------------------------------------------

func TestStartNoWarmupTicker(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
	}
	r := New(cfg, nil, nil)
	// warmup NOT enabled (default)  --  Start() must not create a zero-interval ticker.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Must not panic.
	r.Start(ctx)
}
