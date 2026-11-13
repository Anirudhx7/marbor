package router

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

func newQueueRouter(nodeURL string, maxDepth, timeoutMs int) *Router {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        maxDepth,
		QueueTimeoutMs:       timeoutMs,
	}
	nodeCfg := []config.NodeConfig{{Name: "n1", URL: nodeURL}}
	return New(cfg, nodeCfg, nil)
}

// TestWaitForNodeImmediateRoute verifies the fast path: an available node is
// returned without entering the queue loop.
func TestWaitForNodeImmediateRoute(t *testing.T) {
	r := newQueueRouter("http://localhost:11434", 10, 500)
	// Mark node healthy manually (pollNode would do this in production).
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = true
	n.mu.Unlock()

	node, _, _ := r.WaitForNode(context.Background(), "llama3.2", "", "")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
}

// TestWaitForNodeUnblocksOnDecrConn verifies that a waiter wakes up when a
// connection is released via DecrConn.
func TestWaitForNodeUnblocksOnDecrConn(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        10,
		QueueTimeoutMs:       2000,
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = true
	n.mu.Unlock()

	// Saturate the node with one active connection (simulating a request in flight).
	atomic.StoreInt32(&n.ActiveConns, 1)

	var result *NodeState
	done := make(chan struct{})
	go func() {
		// WaitForNode blocks because we'll pretend there is a global conn limit.
		// Since we have no real conn limit in the router, it will actually route
		// immediately - so this tests the signal path by holding the node unhealthy,
		// then marking healthy and signaling.
		n.mu.Lock()
		n.Healthy = false
		n.mu.Unlock()

		result, _, _ = r.WaitForNode(context.Background(), "llama3.2", "", "")
		close(done)
	}()

	// Small delay to let the goroutine enter the wait loop.
	time.Sleep(50 * time.Millisecond)

	// Mark healthy and signal via DecrConn.
	n.mu.Lock()
	n.Healthy = true
	n.mu.Unlock()
	r.DecrConn(n)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("WaitForNode did not unblock after DecrConn within 1s")
	}
	if result == nil {
		t.Error("expected non-nil node after DecrConn signal")
	}
}

// TestWaitForNodeTimeout verifies that WaitForNode returns nil after the
// configured queue timeout when no node becomes available.
func TestWaitForNodeTimeout(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        10,
		QueueTimeoutMs:       200, // 200ms timeout - fast for tests
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = false // keep unhealthy for the duration
	n.mu.Unlock()

	start := time.Now()
	node, _, _ := r.WaitForNode(context.Background(), "llama3.2", "", "")
	elapsed := time.Since(start)

	if node != nil {
		t.Error("expected nil node on timeout, got non-nil")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned too fast (%v), expected ~200ms timeout", elapsed)
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("returned too slow (%v), timeout should be ~200ms", elapsed)
	}
}

// TestWaitForNodeOverflowSLACapsWaitBelowQueueTimeout verifies that a
// configured overflow_sla_ms shortens the effective wait below the longer
// queue_timeout_ms, so a request falls through to cloud fallback sooner
// under sustained local saturation.
func TestWaitForNodeOverflowSLACapsWaitBelowQueueTimeout(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:       "warm-first",
		QueueMaxDepth:  10,
		QueueTimeoutMs: 5000, // long queue timeout
		OverflowSLAMs:  150,  // SLA is much shorter - should win
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.MaxInFlight = 1
	n.mu.Unlock()
	atomic.StoreInt32(&n.ActiveConns, 1)

	start := time.Now()
	res := r.WaitForNodeWithPrefixOpts(context.Background(), "llama3.2", "", "", "", WaitOpts{CloudUsable: func() bool { return true }})
	elapsed := time.Since(start)

	if res.Node != nil {
		t.Error("expected nil node (still saturated), got non-nil")
	}
	if res.Outcome != WaitSLAExpired {
		t.Errorf("outcome = %q, want %q", res.Outcome, WaitSLAExpired)
	}
	if elapsed >= 1*time.Second {
		t.Errorf("elapsed = %v, expected the ~150ms overflow SLA to cap the wait well under the 5s queue timeout", elapsed)
	}
}

// TestWaitForNodeOverflowSLADisabledByDefault verifies that with
// overflow_sla_ms unset (0), the full queue_timeout_ms still applies -
// existing behavior for anyone who hasn't opted in.
func TestWaitForNodeOverflowSLADisabledByDefault(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:       "warm-first",
		QueueMaxDepth:  10,
		QueueTimeoutMs: 200,
		// OverflowSLAMs left at 0 (disabled).
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = false
	n.mu.Unlock()

	start := time.Now()
	node, _, _ := r.WaitForNode(context.Background(), "llama3.2", "", "")
	elapsed := time.Since(start)

	if node != nil {
		t.Error("expected nil node (still unhealthy), got non-nil")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned too fast (%v), expected the full ~200ms queue timeout with overflow SLA disabled", elapsed)
	}
}

// TestWaitForNodeContextCancel verifies that WaitForNode respects context
// cancellation and returns immediately when the caller disconnects.
func TestWaitForNodeContextCancel(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        10,
		QueueTimeoutMs:       30000, // long timeout; context cancel must win
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = false
	n.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.WaitForNode(ctx, "llama3.2", "", "")
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WaitForNode did not respect context cancel within 500ms")
	}
}

// TestWaitForNodeQueueFullRejectsImmediately verifies that when the queue is at
// max depth, new requests are rejected immediately without queuing.
func TestWaitForNodeQueueFullRejectsImmediately(t *testing.T) {
	cfg := config.RoutingConfig{
		Strategy:             "warm-first",
		PollIntervalMs:       2000,
		Fallback:             "least-connections",
		UpstreamTimeoutMs:    120000,
		MaxRetries:           2,
		SessionAffinityTTL:   "10m",
		NvidiaPollIntervalMs: 30000,
		QueueMaxDepth:        1,
		QueueTimeoutMs:       5000,
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = false
	n.mu.Unlock()

	// Manually pin queueDepth at the limit.
	atomic.StoreInt32(&r.queueDepth, 1)
	defer atomic.StoreInt32(&r.queueDepth, 0)

	start := time.Now()
	node, _, _ := r.WaitForNode(context.Background(), "llama3.2", "", "")
	elapsed := time.Since(start)

	if node != nil {
		t.Error("expected nil when queue full")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("queue-full should reject instantly, took %v", elapsed)
	}
}

// newUnroutableQueueRouter returns a router whose only node is unhealthy, so
// every WaitForNode call has to wait for its timer.
func newUnroutableQueueRouter(maxDepth, timeoutMs int) *Router {
	r := newQueueRouter("http://localhost:11434", maxDepth, timeoutMs)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = false
	n.mu.Unlock()
	return r
}

// saturate makes the only node healthy but full, a waitable state.
func saturate(r *Router) {
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = true
	n.MaxInFlight = 1
	n.mu.Unlock()
	atomic.StoreInt32(&n.ActiveConns, 1)
}

// timedCloudWait is timedWait for a request that may use cloud.
func timedCloudWait(r *Router) time.Duration {
	start := time.Now()
	r.WaitForNodeWithPrefixOpts(context.Background(), "llama3.2", "", "", "", WaitOpts{CloudUsable: func() bool { return true }})
	return time.Since(start)
}

func timedWait(r *Router) time.Duration {
	start := time.Now()
	r.WaitForNode(context.Background(), "llama3.2", "", "")
	return time.Since(start)
}

// A queue timeout changed after construction applies to the next waiter.
func TestSetQueueTimeoutAppliesLive(t *testing.T) {
	r := newUnroutableQueueRouter(10, 5000)
	r.SetQueueTimeout(150)
	if d := timedWait(r); d < 100*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("waited %v after SetQueueTimeout(150), want about 150ms", d)
	}
}

// An overflow SLA set after construction caps the wait for the next waiter.
func TestSetOverflowSLAAppliesLive(t *testing.T) {
	r := newUnroutableQueueRouter(10, 5000)
	saturate(r)
	r.SetOverflowSLA(150)
	if d := timedCloudWait(r); d < 100*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("waited %v after SetOverflowSLA(150), want about 150ms", d)
	}
}

// An SLA of zero means no cap beyond the queue timeout, including when it is
// reset from a non-zero value.
func TestSetOverflowSLAZeroUsesQueueTimeout(t *testing.T) {
	r := newUnroutableQueueRouter(10, 300)
	saturate(r)
	r.SetOverflowSLA(50)
	r.SetOverflowSLA(0)
	if d := timedCloudWait(r); d < 250*time.Millisecond {
		t.Fatalf("waited %v with SLA reset to 0, want the 300ms queue timeout", d)
	}
}

// Setting the depth or the timeout to zero turns the queue off at once.
func TestSetQueueMaxDepthZeroDisablesQueue(t *testing.T) {
	r := newUnroutableQueueRouter(10, 5000)
	r.SetQueueMaxDepth(0)
	if d := timedWait(r); d > 200*time.Millisecond {
		t.Fatalf("waited %v with depth 0, want an immediate return", d)
	}
	r.SetQueueMaxDepth(10)
	r.SetQueueTimeout(0)
	if d := timedWait(r); d > 200*time.Millisecond {
		t.Fatalf("waited %v with timeout 0, want an immediate return", d)
	}
}

// A waiter that is already queued keeps the timer it started with when the
// settings change underneath it.
func TestQueuedWaiterKeepsItsTimer(t *testing.T) {
	r := newUnroutableQueueRouter(10, 400)
	done := make(chan time.Duration, 1)
	go func() { done <- timedWait(r) }()
	time.Sleep(50 * time.Millisecond)
	r.SetQueueTimeout(5000)
	r.SetOverflowSLA(5000)
	select {
	case d := <-done:
		if d < 300*time.Millisecond || d > 1500*time.Millisecond {
			t.Fatalf("queued waiter returned after %v, want about its original 400ms", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued waiter did not return on its original timer")
	}
}

// Lowering the depth below the number already waiting turns new arrivals away
// without evicting anyone.
func TestSetQueueMaxDepthLowerKeepsWaiters(t *testing.T) {
	r := newUnroutableQueueRouter(10, 600)
	done := make(chan time.Duration, 1)
	go func() { done <- timedWait(r) }()
	time.Sleep(50 * time.Millisecond)
	r.SetQueueMaxDepth(1)
	if d := timedWait(r); d > 200*time.Millisecond {
		t.Fatalf("new arrival waited %v with the queue full, want an immediate return", d)
	}
	select {
	case d := <-done:
		if d < 400*time.Millisecond {
			t.Fatalf("existing waiter was evicted after %v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("existing waiter did not return")
	}
}

// Setters racing with many waiters must be clean under -race.
func TestQueueSettersRaceWithWaiters(t *testing.T) {
	r := newUnroutableQueueRouter(100, 200)
	var wg sync.WaitGroup
	var returned atomic.Int32
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timedWait(r)
			returned.Add(1)
		}()
	}
	for i := 0; i < 200; i++ {
		r.SetQueueTimeout(100 + i%200)
		r.SetOverflowSLA(i % 150)
		r.SetQueueMaxDepth(50 + i%100)
	}
	wg.Wait()
	if returned.Load() != 40 {
		t.Fatalf("%d of 40 waiters returned", returned.Load())
	}
}

// A depth larger than int32 must be capped, not wrapped negative (which would
// silently disable the queue).
func TestSetQueueMaxDepthClampsOversizedValue(t *testing.T) {
	r := newQueueRouter("http://localhost:11434", 10, 500)
	r.SetQueueMaxDepth(math.MaxInt32 + 5)
	if got := r.queueMaxDepth.Load(); got != math.MaxInt32 {
		t.Fatalf("depth = %d after an oversized value, want %d", got, int32(math.MaxInt32))
	}
	r.SetQueueMaxDepth(-3)
	if got := r.queueMaxDepth.Load(); got != 0 {
		t.Fatalf("depth = %d after a negative value, want 0", got)
	}
}

func TestQueueTimeout_ReflectsSetting(t *testing.T) {
	r := newQueueRouter("http://127.0.0.1:1", 4, 0)
	for _, c := range []struct {
		ms   int
		want time.Duration
	}{
		{1500, 1500 * time.Millisecond},
		{0, 0},
		{-5, 0},
	} {
		r.SetQueueTimeout(c.ms)
		if got := r.QueueTimeout(); got != c.want {
			t.Errorf("SetQueueTimeout(%d): QueueTimeout() = %v, want %v", c.ms, got, c.want)
		}
	}
}
