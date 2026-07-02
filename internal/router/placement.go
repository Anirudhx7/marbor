package router

// placement.go - Placement and routing-decision logic.
//
// Contains the core weighted scoring and selection logic that Route and
// RouteExcluding delegate to. Extracted from router.go and updated in Step 4
// to support multi-factor placement scoring, model pinning, and node cooldown.

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

// computeNodeScore calculates a multi-factor score for a node.
// score = (warm_model_resident * 50) + (free_vram_headroom * 20) +
//
//	(inverse_queue_depth * 15) + (node_health_score * 10) +
//	(recent_success_rate * 5)
func (r *Router) computeNodeScore(n *NodeState, model string) float64 {
	n.mu.RLock()
	defer n.mu.RUnlock()

	// 1. warm_model_resident
	warm := 0.0
	for _, m := range n.LoadedModels {
		if m.Name == model {
			warm = 1.0
			break
		}
	}

	// 2. free_vram_headroom
	freeVRAM := 0.0
	if n.VRAMTotalMB > 0 {
		free := n.VRAMTotalMB - n.VRAMUsedMB
		if free > 0 {
			freeVRAM = float64(free) / float64(n.VRAMTotalMB)
		}
	}

	// 3. inverse_queue_depth
	conns := atomic.LoadInt32(&n.ActiveConns)
	invQueue := 1.0 / (1.0 + float64(conns))

	// 4. node_health_score
	health := 1.0
	if len(n.HealthHistory) > 0 {
		sum := 0.0
		for _, h := range n.HealthHistory {
			sum += h
		}
		health = sum / (100.0 * float64(len(n.HealthHistory)))
	}

	// 5. recent_success_rate
	success := 1.0
	if len(n.SuccessHistory) > 0 {
		sum := 0.0
		for _, s := range n.SuccessHistory {
			if s {
				sum += 1.0
			}
		}
		success = sum / float64(len(n.SuccessHistory))
	}

	score := (warm * 50.0) + (freeVRAM * 20.0) + (invQueue * 15.0) + (health * 10.0) + (success * 5.0)

	// Cooldown penalty: reduce node score by 50 points if in 60s cooldown
	if !n.LastErrorAt.IsZero() && time.Since(n.LastErrorAt) < 60*time.Second {
		score -= 50.0
		if score < 0 {
			score = 0
		}
	}

	return score
}

// findBestByScore finds the best node from the given slice based on weighted score,
// using alphabetical order of Name as a deterministic tiebreaker.
func (r *Router) findBestByScore(nodes []*NodeState, modelName string) *NodeState {
	var bestNode *NodeState
	var bestScore float64 = -999.0

	for _, n := range nodes {
		score := r.computeNodeScore(n, modelName)
		if bestNode == nil {
			bestNode = n
			bestScore = score
			continue
		}
		if score > bestScore {
			bestNode = n
			bestScore = score
		} else if score == bestScore {
			if n.Name < bestNode.Name {
				bestNode = n
				bestScore = score
			}
		}
	}
	return bestNode
}

// selectBestNode runs scoring and handles pinned models.
// If the model is pinned and warm on any healthy candidate, it is selected immediately.
func (r *Router) selectBestNode(candidates []*NodeState, modelName string) (*NodeState, bool) {
	if len(candidates) == 0 {
		return nil, false
	}

	// 1. Check pinned & warm nodes first
	if modelName != "" {
		var pinnedAndWarm []*NodeState
		for _, n := range candidates {
			if r.isPinned(n.Name, modelName) && isModelWarm(n, modelName) {
				pinnedAndWarm = append(pinnedAndWarm, n)
			}
		}
		if len(pinnedAndWarm) > 0 {
			bestNode := r.findBestByScore(pinnedAndWarm, modelName)
			metrics.CacheHit()
			return bestNode, true
		}
	}

	// 2. Score all candidates
	bestNode := r.findBestByScore(candidates, modelName)
	if bestNode == nil {
		return nil, false
	}
	warm := isModelWarm(bestNode, modelName)
	if warm {
		metrics.CacheHit()
	} else {
		metrics.CacheMiss()
	}
	return bestNode, warm
}

// routeInternal is the core weighted selection logic that Route delegates to.
func (r *Router) routeInternal(modelName, runtimeFilter string) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
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
	return r.selectBestNode(healthy, modelName)
}

// Route picks the best healthy node for modelName using weighted placement scoring.
// If sessionID is non-empty and a valid affinity entry exists for it, the
// previously-used node is preferred (sticky session).
func (r *Router) Route(modelName, sessionID, runtimeFilter string) (*NodeState, bool) {
	if !r.sessionAffinity {
		sessionID = ""
	}
	if sessionID != "" {
		if node := r.stickyNode(sessionID); node != nil {
			if runtimeFilter == "" || node.Runtime == runtimeFilter {
				r.RecordTransition(modelName, time.Now())
				return node, isModelWarm(node, modelName)
			}
			r.affinityMu.Lock()
			delete(r.affinity, sessionID)
			r.affinityMu.Unlock()
		}
	}

	node, warm := r.routeInternal(modelName, runtimeFilter)
	if node != nil {
		r.RecordTransition(modelName, time.Now())
		if sessionID != "" {
			r.affinityMu.Lock()
			if len(r.affinity) < maxAffinityEntries {
				entry := &affinityEntry{nodeURL: node.URL}
				entry.lastSeen.Store(time.Now().UnixNano())
				r.affinity[sessionID] = entry
			}
			r.affinityMu.Unlock()
		}
	}
	return node, warm
}

// RouteExcluding picks the best healthy node for modelName using weighted placement scoring,
// excluding any node whose URL appears in the exclude map.
func (r *Router) RouteExcluding(modelName, runtimeFilter string, exclude map[string]bool) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
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
	return r.selectBestNode(healthy, modelName)
}

// pickLeastConns returns the node with the fewest active connections.
// Retained for backwards compatibility in existing tests.
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
// Retained for backwards compatibility in existing tests.
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
