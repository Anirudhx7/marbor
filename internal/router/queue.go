package router

// queue.go - Connection-tracking queue and WaitForNode implementation.

import (
	"math"
	"sync/atomic"

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
// model on a node, alongside the per-node IncrConn. Consulted by
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

// QueueDepth returns the current number of requests waiting in WaitForNode.
func (r *Router) QueueDepth() int {
	return int(atomic.LoadInt32(&r.queueDepth))
}

// SetQueueTimeout changes how long a queued request waits for a node, in
// milliseconds. Zero or negative disables the queue. It applies to requests
// that enter the queue after the call; requests already waiting keep the
// timer they started with.
func (r *Router) SetQueueTimeout(ms int) {
	r.queueTimeoutMs.Store(int64(max(ms, 0)))
}

// SetQueueMaxDepth changes how many requests may wait at once. Zero or
// negative disables the queue. Lowering it below the current depth turns new
// arrivals away until it drains; it never evicts a request already waiting.
func (r *Router) SetQueueMaxDepth(n int) {
	r.queueMaxDepth.Store(clampQueueDepth(n))
}

// SetOverflowSLA changes the cap on how long a request waits for local
// capacity before cloud fallback, in milliseconds. Zero or negative means no
// cap beyond the queue timeout. It applies to requests that enter the queue
// after the call.
func (r *Router) SetOverflowSLA(ms int) {
	r.overflowSLAMs.Store(int64(max(ms, 0)))
}

// clampQueueDepth converts a configured depth to the int32 the queue stores.
// A value that does not fit is capped rather than wrapped: a wrapped negative
// would read as "queue disabled". Zero or negative means disabled.
func clampQueueDepth(n int) int32 {
	if n <= 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}
