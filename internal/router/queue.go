package router

// queue.go - Connection-tracking queue and WaitForNode implementation.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
)

func (r *Router) IncrConn(node *NodeState) {
	if node != nil {
		v := atomic.AddInt32(&node.ActiveConns, 1)
		atomic.AddInt64(&node.RequestsTotal, 1)
		metrics.ActiveConnections(node.Name, float64(v))
	}
}

func (r *Router) DecrConn(node *NodeState) {
	if node != nil {
		v := atomic.AddInt32(&node.ActiveConns, -1)
		if v < 0 {
			atomic.StoreInt32(&node.ActiveConns, 0)
			v = 0
		}
		metrics.ActiveConnections(node.Name, float64(v))
		// Wake up all WaitForNode callers - a slot just freed.
		r.notifyMu.Lock()
		ch := r.notifyCh
		r.notifyCh = make(chan struct{})
		close(ch)
		r.notifyMu.Unlock()
	}
}

// IncrModelInFlight increments the in-flight request count for a specific
// model on a node (P117), alongside the per-node IncrConn. Consulted by
// EvictForHeadroom's victim selection to protect an actively-serving model
// from being picked as the coldest LRU candidate mid-generation.
func (r *Router) IncrModelInFlight(node *NodeState, model string) {
	if node == nil || model == "" {
		return
	}
	node.mu.Lock()
	if node.modelInFlight == nil {
		node.modelInFlight = make(map[string]int32)
	}
	node.modelInFlight[model]++
	node.mu.Unlock()
}

// DecrModelInFlight decrements the in-flight count set by IncrModelInFlight,
// removing the entry once it reaches zero so modelInFlight doesn't grow
// unbounded with stale zero-value entries for models no longer in use.
func (r *Router) DecrModelInFlight(node *NodeState, model string) {
	if node == nil || model == "" {
		return
	}
	node.mu.Lock()
	if v, ok := node.modelInFlight[model]; ok {
		if v <= 1 {
			delete(node.modelInFlight, model)
		} else {
			node.modelInFlight[model] = v - 1
		}
	}
	node.mu.Unlock()
}

// WaitForNode is the queued variant of Route. It first tries Route() immediately;
// if no node is available it waits up to queueTimeout for one to free up (signaled
// by DecrConn). Returns nil after timeout or context cancellation, at which point
// the caller should fall through to cloud fallback or 503.
//
// runtimeFilter, when non-empty, restricts candidates to nodes whose Runtime
// matches exactly (e.g. "ollama"). Pass "" to allow any runtime.
//
// If the queue is already at queueMaxDepth, returns nil immediately without queuing
// to prevent unbounded memory growth under sustained overload.
func (r *Router) WaitForNode(ctx context.Context, modelName, sessionID, runtimeFilter string) (*NodeState, bool, *RoutingDecision) {
	return r.WaitForNodeWithPrefix(ctx, modelName, sessionID, runtimeFilter, "")
}

// WaitForNodeWithPrefix is WaitForNode plus an optional prefix-locality hint
// (Step 6) - see RouteWithPrefix. Pass prefixHash == "" for identical
// behavior to WaitForNode.
func (r *Router) WaitForNodeWithPrefix(ctx context.Context, modelName, sessionID, runtimeFilter, prefixHash string) (*NodeState, bool, *RoutingDecision) {
	// Fast path: immediate route.
	if node, warm, decision := r.RouteWithPrefix(modelName, sessionID, runtimeFilter, prefixHash); node != nil {
		return node, warm, decision
	}

	// Queue disabled (timeout or depth == 0): fall through immediately.
	// config.Validate() sets the production defaults; callers that bypass
	// Validate() (unit tests, zero-config New()) get no queue.
	if r.queueTimeout <= 0 || r.queueMaxDepth <= 0 {
		return nil, false, nil
	}

	// Claim a queue slot atomically. Reject if already at capacity.
	depth := atomic.AddInt32(&r.queueDepth, 1)
	if int(depth) > r.queueMaxDepth {
		atomic.AddInt32(&r.queueDepth, -1)
		return nil, false, nil
	}
	// Publish via a fresh load, not the locally-captured `depth` return
	// value: two concurrent waiters' AddInt32-then-publish pairs can
	// interleave (e.g. B's increment+publish lands between A's increment
	// and A's publish), so publishing A's own stale captured value after
	// B's already-fresher publish would leave the exported gauge showing a
	// value other than the true current depth.
	metrics.QueueDepth(float64(atomic.LoadInt32(&r.queueDepth)))
	defer func() {
		atomic.AddInt32(&r.queueDepth, -1)
		metrics.QueueDepth(float64(atomic.LoadInt32(&r.queueDepth)))
	}()

	// SLA-driven cloud overflow: an operator-set overflow_sla_ms caps how long
	// this request waits for local capacity before falling through to cloud
	// fallback (or 503), overriding the longer queue_timeout_ms for that
	// purpose only. This never changes which nodes Route() considers - it is
	// a queue-wait timing knob, not a Hard-Constraint bypass.
	waitTimeout := r.queueTimeout
	if r.overflowSLA > 0 && r.overflowSLA < waitTimeout {
		waitTimeout = r.overflowSLA
	}
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	// Periodic safety-net retry: coalesced notifyCh signals can miss concurrent
	// DecrConn bursts (channel capacity 1). 500ms poll is the fallback safety net;
	// immediate wakeups are handled via the notifyCh channel (#15).
	retryTick := time.NewTicker(500 * time.Millisecond)
	defer retryTick.Stop()

	for {
		r.notifyMu.Lock()
		ch := r.notifyCh
		r.notifyMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-timer.C:
			metrics.QueueTimeout()
			return nil, false, nil
		case <-ch:
			if node, warm, decision := r.RouteWithPrefix(modelName, sessionID, runtimeFilter, prefixHash); node != nil {
				return node, warm, decision
			}
		case <-retryTick.C:
			if node, warm, decision := r.RouteWithPrefix(modelName, sessionID, runtimeFilter, prefixHash); node != nil {
				return node, warm, decision
			}
		}
	}
}

// QueueDepth returns the current number of requests waiting in WaitForNode.
func (r *Router) QueueDepth() int {
	return int(atomic.LoadInt32(&r.queueDepth))
}
