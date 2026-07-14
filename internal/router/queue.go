package router

// queue.go - Connection-tracking queue and WaitForNode implementation.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
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
		// Wake up all WaitForNode callers  --  a slot just freed.
		r.notifyMu.Lock()
		ch := r.notifyCh
		r.notifyCh = make(chan struct{})
		close(ch)
		r.notifyMu.Unlock()
	}
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
func (r *Router) WaitForNode(ctx context.Context, modelName, sessionID, runtimeFilter string) (*NodeState, bool) {
	// Fast path: immediate route.
	if node, warm := r.Route(modelName, sessionID, runtimeFilter); node != nil {
		return node, warm
	}

	// Queue disabled (timeout or depth == 0): fall through immediately.
	// config.Validate() sets the production defaults; callers that bypass
	// Validate() (unit tests, zero-config New()) get no queue.
	if r.queueTimeout <= 0 || r.queueMaxDepth <= 0 {
		return nil, false
	}

	// Claim a queue slot atomically. Reject if already at capacity.
	depth := atomic.AddInt32(&r.queueDepth, 1)
	if int(depth) > r.queueMaxDepth {
		atomic.AddInt32(&r.queueDepth, -1)
		return nil, false
	}
	metrics.QueueDepth(float64(depth))
	defer func() {
		d := atomic.AddInt32(&r.queueDepth, -1)
		metrics.QueueDepth(float64(d))
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
			return nil, false
		case <-timer.C:
			metrics.QueueTimeout()
			return nil, false
		case <-ch:
			if node, warm := r.Route(modelName, sessionID, runtimeFilter); node != nil {
				return node, warm
			}
		case <-retryTick.C:
			if node, warm := r.Route(modelName, sessionID, runtimeFilter); node != nil {
				return node, warm
			}
		}
	}
}

// QueueDepth returns the current number of requests waiting in WaitForNode.
func (r *Router) QueueDepth() int {
	return int(atomic.LoadInt32(&r.queueDepth))
}
