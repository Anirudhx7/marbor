package router

// placement.go - Placement and routing-decision logic.
//
// Contains the core weighted scoring and selection logic that Route and
// RouteExcluding delegate to. Extracted from router.go and updated in Step 4
// to support multi-factor placement scoring, model pinning, and node cooldown.

import (
	"log"
	"math"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// recordModelDigest remembers the first non-empty digest observed for a
// model name, across all nodes. A later observation of a DIFFERENT non-empty
// digest under the same name is not overwritten here - it's surfaced via
// digestMismatch so placement scoring can react to it, not silently adopted
// as the new "truth" (there's no way to know which node has the "right"
// weights, only that they disagree).
func (r *Router) recordModelDigest(name, digest string) {
	if name == "" || digest == "" {
		return
	}
	r.digestMu.Lock()
	if _, ok := r.modelDigests[name]; !ok {
		r.modelDigests[name] = digest
	}
	r.digestMu.Unlock()
}

// digestMismatch reports whether digest is known to differ from the
// first-observed digest recorded for name. Always false when either side is
// empty - a runtime that doesn't report a digest (anything but Ollama today,
// per ModelInfo.Digest) or a name with no digest recorded yet is never
// flagged, matching R1 (never fabricate a comparison from missing data).
func (r *Router) digestMismatch(name, digest string) bool {
	if name == "" || digest == "" {
		return false
	}
	r.digestMu.RLock()
	known, ok := r.modelDigests[name]
	r.digestMu.RUnlock()
	return ok && known != digest
}

// isModelWarm reports whether modelName is currently loaded in VRAM on node n
// with a digest that doesn't conflict with another node's copy under the
// same name. A loaded model whose digest mismatches the first-observed
// digest for modelName is NOT counted as warm here - crediting it as an
// interchangeable warm hit would silently mix two different sets of weights
// under one model name (see .local/audit-fixes-2026-08-03.md #4).
func (r *Router) isModelWarm(n *NodeState, modelName string) bool {
	if modelName == "" {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, m := range n.LoadedModels {
		if m.Name == modelName && !r.digestMismatch(m.Name, m.Digest) {
			return true
		}
	}
	return false
}

// isEligibleForModel reports whether n may be a routing candidate for
// modelName at all (P79 hard eligibility filter, Routing Hierarchy step 1).
// Ollama is exempt: its runtime can load/pull a requested model on demand, so
// absence from LoadedModels does not disqualify it (existing cold-start
// behavior, unchanged). Every other runtime has no such on-demand load path
// from the mesh's perspective, so a non-Ollama node must already report
// modelName in LoadedModels - otherwise a healthy-but-wrong-model node could
// silently serve output from a different model than the one requested (see
// req-af404f8a, P79 filing in EXECUTION-QUEUE.md).
func (r *Router) isEligibleForModel(n *NodeState, modelName string) bool {
	if modelName == "" {
		return true
	}
	if runtime := n.GetRuntime(); runtime == "" || runtime == "ollama" {
		return true
	}
	return r.isModelWarm(n, modelName)
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

// FlushAffinity snapshots the current in-memory affinity map to the store, so
// a restart doesn't drop every in-flight sticky session and force a cold
// KV-cache round-trip on the next request for each of them (see
// .local/audit-fixes-2026-08-03.md #7). Called on the same periodic cadence
// as sweepAffinity and once more at shutdown, alongside FlushWarmState. A nil
// store (tests, or persistence disabled) makes this a no-op. Best-effort - a
// store error is logged and swallowed, matching every other warm/affinity
// persistence path in this package.
func (r *Router) FlushAffinity() {
	st := r.warmStore()
	if st == nil {
		return
	}
	r.affinityMu.RLock()
	entries := make([]store.AffinityRecord, 0, len(r.affinity))
	for id, e := range r.affinity {
		entries = append(entries, store.AffinityRecord{
			SessionID: id,
			NodeURL:   e.nodeURL,
			LastSeen:  time.Unix(0, e.lastSeen.Load()),
		})
	}
	r.affinityMu.RUnlock()
	if err := st.SnapshotAffinity(entries); err != nil {
		log.Printf("affinity: flush: %v", err)
	}
}

// RestoreAffinity seeds the in-memory affinity map from the store at startup,
// so sticky sessions survive a mesh restart. Entries already past the TTL
// window are skipped rather than restored and immediately swept - Route
// still re-validates health/draining before honoring any restored entry,
// exactly as it does for one created during normal operation. Returns the
// number of entries restored. Call after nodes are registered and before
// serving client traffic, alongside RestoreWarmState.
func (r *Router) RestoreAffinity() (int, error) {
	st := r.warmStore()
	if st == nil {
		return 0, nil
	}
	rows, err := st.AllAffinity()
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	restored := 0
	r.affinityMu.Lock()
	if r.affinity == nil {
		r.affinity = make(map[string]*affinityEntry)
	}
	for _, w := range rows {
		if w.LastSeen.IsZero() || time.Since(w.LastSeen) >= r.affinityTTL {
			continue
		}
		if len(r.affinity) >= maxAffinityEntries {
			break
		}
		entry := &affinityEntry{nodeURL: w.NodeURL}
		entry.lastSeen.Store(w.LastSeen.UnixNano())
		r.affinity[w.SessionID] = entry
		restored++
	}
	r.affinityMu.Unlock()
	return restored, nil
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

// staticVRAMReservation reports whether runtime statically pre-allocates
// GPU memory at startup (weights + KV cache), making VRAMUsedMB a
// by-design constant rather than a live pressure signal (P62). Confirmed
// today for vLLM only (gpu-memory-utilization); TGI has a static mode too
// but that is not yet confirmed against this mesh's telemetry, so it is
// deliberately NOT included here - narrowing to what's verified, not
// guessing across the runtime matrix (Architecture Law 5). Ollama,
// llama.cpp, and MLX dynamically allocate and are unaffected.
func staticVRAMReservation(runtime string) bool {
	return runtime == "vllm"
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
		// Skip a loaded copy whose digest is known to conflict with another
		// node's copy under the same name - crediting it as an
		// interchangeable warm hit would silently mix variants. See
		// isModelWarm's doc comment.
		if m.Name == model && !r.digestMismatch(m.Name, m.Digest) {
			warm = 1.0
			break
		}
	}

	// 2. free_vram_headroom
	freeVRAM := 0.0
	if staticVRAMReservation(n.Runtime) {
		// vLLM (gpu-memory-utilization, commonly 0.8-0.9) statically
		// pre-allocates weights + KV cache at startup, so VRAMUsedMB sits at
		// ~90%+ by design even when the node is idle and fully able to
		// serve (P62). Instantaneous free bytes is not a capacity signal for
		// these runtimes - do not credit or penalize the reserved block.
		// Use the same real, already-tracked in-flight request count that
		// drives factor 3 (inverse_queue_depth) as the "capacity to accept
		// another request" signal instead; no runtime today reports a
		// usable queue-depth or declared-concurrency telemetry field
		// (nodeagent.RuntimeInfo.QueueDepth is never populated - R1).
		conns := atomic.LoadInt32(&n.ActiveConns)
		freeVRAM = 1.0 / (1.0 + float64(conns))
	} else if n.VRAMTotalMB > 0 {
		free := n.VRAMTotalMB - n.VRAMUsedMB
		// Discount VRAM already reserved-but-unconfirmed by an in-flight
		// cold-start pick on this node (see reserveColdStartBytes) - without
		// this, two concurrent requests for two different cold models can
		// both see the same stale last-poll snapshot and both pick this node
		// before either load is confirmed by the next poll (P51).
		if reserved := r.PendingPrewarmBytes(n.Name); reserved > 0 {
			free -= reserved / (1024 * 1024)
		}
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

	// Stale-telemetry penalty: markFailure only flips Healthy false after
	// healthFailureThreshold CONSECUTIVE poll failures (health.go), so a node
	// that just crashed keeps scoring as if its last-known VRAM/queue/loaded-
	// models snapshot were still current for that whole grace window. Once a
	// node's poll data is older than the grace window a healthy node's poll
	// cadence would have refreshed it by, apply the same -50 penalty as the
	// error cooldown above rather than trusting a snapshot that's actually
	// gone stale. See .local/audit-fixes-2026-08-03.md #3.
	if !n.LastPollAt.IsZero() && r.interval > 0 {
		staleAfter := time.Duration(r.healthFailureThreshold) * r.interval
		if time.Since(n.LastPollAt) > staleAfter {
			score -= 50.0
			if score < 0 {
				score = 0
			}
		}
	}

	return score
}

// findBestByScore finds the best node from the given slice based on weighted score,
// using alphabetical order of Name as a deterministic tiebreaker.
//
// It also holds a provisional cold-start VRAM reservation on whichever node is
// the leading candidate at each point in the loop (see reserveColdStartBytes),
// moving it to the new leader whenever a later candidate displaces the current
// one. This narrows - it does not eliminate - the score-read vs
// reservation-write race between two concurrent Route() calls for different
// cold models: previously the reservation for the eventual winner was only
// written after every candidate had been scored (selectBestNode, post-P51),
// so a concurrent call scoring the same node saw stale headroom for the
// entire scoring pass, not just the time after this candidate became the
// leader. See .local/audit-fixes-2026-08-03.md #2.
func (r *Router) findBestByScore(nodes []*NodeState, modelName string) *NodeState {
	var bestNode *NodeState
	var bestScore float64 = -999.0
	var reservedFor *NodeState // node currently holding this loop's provisional reservation, if any

	for _, n := range nodes {
		score := r.computeNodeScore(n, modelName)
		isNewBest := bestNode == nil || score > bestScore || (score == bestScore && n.Name < bestNode.Name)
		if !isNewBest {
			continue
		}
		bestNode = n
		bestScore = score

		if reservedFor != nil && reservedFor != n {
			r.clearWarmReservation(reservedFor.Name, modelName)
			reservedFor = nil
		}
		if modelName != "" && !r.isModelWarm(n, modelName) {
			r.reserveColdStartBytes(n.URL, n.Name, modelName)
			reservedFor = n
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
			if r.isPinned(n.Name, modelName) && r.isModelWarm(n, modelName) {
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
	warm := r.isModelWarm(bestNode, modelName)
	if warm {
		metrics.CacheHit()
	} else {
		metrics.CacheMiss()
		// findBestByScore already holds a provisional reservation on
		// bestNode by the time it returns (it reserves on whichever
		// candidate is the leader as the loop proceeds, not just at the
		// end) - no separate reserve call needed here.
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
		if runtimeFilter != "" && n.GetRuntime() != runtimeFilter {
			continue // skip nodes that don't match the requested runtime
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		n.mu.RUnlock()
		if isHealthy && !isDraining && r.isEligibleForModel(n, modelName) {
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
			if (runtimeFilter == "" || node.GetRuntime() == runtimeFilter) && r.isEligibleForModel(node, modelName) {
				r.RecordTransition(modelName, time.Now())
				warm := r.isModelWarm(node, modelName)
				if !warm {
					// Same race guard as selectBestNode's cold-start path -
					// the sticky-session shortcut bypasses selectBestNode
					// entirely, so it needs its own reservation write (P51).
					r.reserveColdStartBytes(node.URL, node.Name, modelName)
				}
				return node, warm
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
		if runtimeFilter != "" && n.GetRuntime() != runtimeFilter {
			continue // skip nodes that don't match the requested runtime
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		n.mu.RUnlock()
		if isHealthy && !isDraining && r.isEligibleForModel(n, modelName) {
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
