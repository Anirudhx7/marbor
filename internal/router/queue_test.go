package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
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

	node, _ := r.WaitForNode(context.Background(), "llama3.2", "")
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
		// immediately — so this tests the signal path by holding the node unhealthy,
		// then marking healthy and signaling.
		n.mu.Lock()
		n.Healthy = false
		n.mu.Unlock()

		result, _ = r.WaitForNode(context.Background(), "llama3.2", "")
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
		QueueTimeoutMs:       200, // 200ms timeout — fast for tests
	}
	r := New(cfg, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.Lock()
	n.Healthy = false // keep unhealthy for the duration
	n.mu.Unlock()

	start := time.Now()
	node, _ := r.WaitForNode(context.Background(), "llama3.2", "")
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
		r.WaitForNode(ctx, "llama3.2", "")
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
	node, _ := r.WaitForNode(context.Background(), "llama3.2", "")
	elapsed := time.Since(start)

	if node != nil {
		t.Error("expected nil when queue full")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("queue-full should reject instantly, took %v", elapsed)
	}
}
