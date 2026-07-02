package router

// placement.go - Placement and routing-decision logic.
//
// Contains the core warm-first / strategy-aware node selection functions that
// Route and RouteExcluding delegate to. Extracted from router.go for clarity;
// all functions remain methods on *Router or package-level helpers — no new
// abstractions, zero behavior change.

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// isModelWarm reports whether modelName is currently loaded in VRAM on node n.
func isModelWarm(n *NodeState, modelName string) bool {
	if modelName == "" {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, m := range n.LoadedModels {
		if m.Name == modelName {
			return true
		}
	}
	return false
}

// pickLeastConns returns the node with the fewest active connections.
func pickLeastConns(nodes []*NodeState) *NodeState {
	var best *NodeState
	minConns := int32(math.MaxInt32)
	for _, n := range nodes {
		conns := atomic.LoadInt32(&n.ActiveConns)
		if conns < minConns {
			minConns = conns
			best = n
		}
	}
	return best
}

// pickMostFreeVRAM selects the healthy node with the most free VRAM.
// Nodes where VRAMTotalMB == 0 (unknown capacity) or VRAMUsedMB >= VRAMTotalMB
// (overcommitted / at capacity) are excluded; if ALL eligible nodes are excluded
// it falls back to pickLeastConns so a request is never dropped.
func pickMostFreeVRAM(nodes []*NodeState) *NodeState {
	var best *NodeState
	var bestFree int64 = 0
	for _, n := range nodes {
		n.mu.RLock()
		total := n.VRAMTotalMB
		used := n.VRAMUsedMB
		n.mu.RUnlock()
		if total <= 0 {
			continue // capacity unknown
		}
		free := total - used
		if free <= 0 {
			continue // at or over capacity
		}
		if free > bestFree {
			bestFree = free
			best = n
		}
	}
	if best == nil {
		return pickLeastConns(nodes) // all unknown/full: safe degradation
	}
	return best
}

// sweepAffinity removes expired session-affinity entries. Called periodically
// from Start to bound memory usage on long-running deployments.
func (r *Router) sweepAffinity() {
	now := time.Now().UnixNano()
	r.affinityMu.Lock()
	for id, e := range r.affinity {
		if now-e.lastSeen.Load() >= int64(r.affinityTTL) {
			delete(r.affinity, id)
		}
	}
	r.affinityMu.Unlock()
}

// stickyNode returns the pinned node for sessionID if it is still healthy and
// within the TTL window, refreshing the TTL on success. Returns nil to signal
// "fall through to normal routing."
func (r *Router) stickyNode(sessionID string) *NodeState {
	r.affinityMu.RLock()
	entry, ok := r.affinity[sessionID]
	var lastSeenNano int64
	var nodeURL string
	if ok {
		// Copy fields while holding the lock to avoid a data race: the struct
		// is heap-allocated and shared; reading fields after RUnlock is unsafe
		// if sweepAffinity or Route concurrently modifies the same entry.
		lastSeenNano = entry.lastSeen.Load()
		nodeURL = entry.nodeURL
	}
	r.affinityMu.RUnlock()
	if !ok || time.Since(time.Unix(0, lastSeenNano)) >= r.affinityTTL {
		return nil
	}

	r.mu.RLock()
	var sticky *NodeState
	for _, n := range r.nodes {
		if n.URL == nodeURL {
			sticky = n
			break
		}
	}
	r.mu.RUnlock()

	if sticky == nil {
		r.affinityMu.Lock()
		if e, ok := r.affinity[sessionID]; ok && time.Since(time.Unix(0, e.lastSeen.Load())) >= r.affinityTTL {
			delete(r.affinity, sessionID)
		}
		r.affinityMu.Unlock()
		return nil
	}
	sticky.mu.RLock()
	healthy := sticky.Healthy
	draining := sticky.Draining
	sticky.mu.RUnlock()
	if !healthy || draining {
		r.affinityMu.Lock()
		if e, ok := r.affinity[sessionID]; ok && time.Since(time.Unix(0, e.lastSeen.Load())) >= r.affinityTTL {
			delete(r.affinity, sessionID)
		}
		r.affinityMu.Unlock()
		return nil
	}

	r.affinityMu.RLock()
	if e, ok := r.affinity[sessionID]; ok {
		e.lastSeen.Store(time.Now().UnixNano())
	}
	r.affinityMu.RUnlock()
	return sticky
}

// routeInternal is the core warm-first / fallback routing logic, extracted so
// both Route and RouteExcluding can share it without duplicating the strategy
// switch. runtimeFilter, when non-empty, restricts candidates to nodes whose
// Runtime matches exactly.
func (r *Router) routeInternal(modelName, runtimeFilter string) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	fallback := r.fallback
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		if runtimeFilter != "" && n.Runtime != runtimeFilter {
			continue // skip nodes that don't match the requested runtime
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		n.mu.RUnlock()
		if isHealthy && !isDraining {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return nil, false
	}

	if modelName != "" && strategy == "warm-first" {
		var warm []*NodeState
		for _, n := range healthy {
			n.mu.RLock()
			for _, m := range n.LoadedModels {
				if m.Name == modelName {
					warm = append(warm, n)
					break
				}
			}
			n.mu.RUnlock()
		}
		if len(warm) > 0 {
			metrics.CacheHit()
			return pickLeastConns(warm), true
		}
		metrics.CacheMiss()
	}

	switch fallback {
	case "vram-aware":
		return pickMostFreeVRAM(healthy), false
	case "round-robin":
		idx := atomic.AddUint32(&r.roundRobin, 1) % uint32(len(healthy))
		return healthy[idx], false
	default: // "least-connections" or ""
		return pickLeastConns(healthy), false
	}
}

// Route picks the best healthy node for modelName. If sessionID is non-empty
// and a valid affinity entry exists for it, the previously-used node is
// preferred (KV-cache / context affinity). If the sticky node is gone or
// unhealthy, the entry is evicted and normal warm-first routing applies; the
// new node is then pinned for the session.
//
// runtimeFilter, when non-empty, restricts candidates to nodes whose Runtime
// field matches exactly. Pass "" to allow any runtime (existing behaviour).
func (r *Router) Route(modelName, sessionID, runtimeFilter string) (*NodeState, bool) {
	// Session affinity is opt-in (routing.session_affinity). When disabled,
	// ignore any client-supplied X-Session-ID so routing is fully stateless —
	// no sticky pinning. Previously the session ID was honored unconditionally,
	// so the config flag had no effect.
	if !r.sessionAffinity {
		sessionID = ""
	}
	if sessionID != "" {
		if node := r.stickyNode(sessionID); node != nil {
			// Honour runtime filter even for sticky nodes.
			if runtimeFilter == "" || node.Runtime == runtimeFilter {
				return node, isModelWarm(node, modelName)
			}
			// Sticky node doesn't match filter — evict and re-route.
			r.affinityMu.Lock()
			delete(r.affinity, sessionID)
			r.affinityMu.Unlock()
		}
	}

	node, warm := r.routeInternal(modelName, runtimeFilter)
	if node != nil && sessionID != "" {
		r.affinityMu.Lock()
		// Only pin if under the cap — prevents memory-exhaustion DoS from
		// authenticated callers sending unique session IDs at high rate.
		if len(r.affinity) < maxAffinityEntries {
			entry := &affinityEntry{nodeURL: node.URL}
			entry.lastSeen.Store(time.Now().UnixNano())
			r.affinity[sessionID] = entry
		}
		r.affinityMu.Unlock()
	}
	return node, warm
}

// RouteExcluding picks the best healthy node for modelName, excluding any
// node whose URL appears in the exclude map. Used by the retry loop in
// proxy.go to avoid re-selecting an already-failed node.
//
// runtimeFilter, when non-empty, additionally restricts candidates to nodes
// whose Runtime field matches exactly. Pass "" to allow any runtime.
func (r *Router) RouteExcluding(modelName, runtimeFilter string, exclude map[string]bool) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	fallback := r.fallback // read here to avoid a second lock acquisition (#17)
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		if exclude[n.URL] {
			continue
		}
		if runtimeFilter != "" && n.Runtime != runtimeFilter {
			continue // skip nodes that don't match the requested runtime
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		n.mu.RUnlock()
		if isHealthy && !isDraining {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return nil, false
	}

	if modelName != "" && strategy == "warm-first" {
		var warm []*NodeState
		for _, n := range healthy {
			n.mu.RLock()
			for _, m := range n.LoadedModels {
				if m.Name == modelName {
					warm = append(warm, n)
					break
				}
			}
			n.mu.RUnlock()
		}
		if len(warm) > 0 {
			return pickLeastConns(warm), true
		}
	}

	switch fallback {
	case "vram-aware":
		return pickMostFreeVRAM(healthy), false
	case "round-robin":
		idx := atomic.AddUint32(&r.roundRobin, 1) % uint32(len(healthy))
		return healthy[idx], false
	default: // "least-connections" or ""
		return pickLeastConns(healthy), false
	}
}
