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

	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/metrics"
	"github.com/Anirudhx7/marbor/internal/store"
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

// reconcileModelDigests re-syncs the stored reference digest for every model
// name once per full poll cycle, fixing the permanent warm-detection
// loss recordModelDigest's first-observation-wins policy otherwise leaves
// behind: without this, once any node reports a second digest for a name,
// digestMismatch returns true forever, even after the entire fleet later
// converges on that new digest (e.g. a re-pull). For each recorded model
// name where no node currently reports the OLD stored digest and at least
// one node reports a new non-empty digest, the stored reference digest is
// replaced with that new one. This is a targeted reconciliation, not a
// change to the routing/scoring formula.
func (r *Router) reconcileModelDigests(nodes []*NodeState) {
	// Snapshot the current reference digests first, so the node scan below
	// can check each observed digest against them without holding digestMu
	// and n.mu at once. isModelWarm already locks n.mu before calling
	// digestMismatch (which takes digestMu); locking digestMu first here
	// would invert that order and risk deadlock under concurrent access.
	r.digestMu.RLock()
	oldDigests := make(map[string]string, len(r.modelDigests))
	for name, digest := range r.modelDigests {
		oldDigests[name] = digest
	}
	r.digestMu.RUnlock()
	if len(oldDigests) == 0 {
		return
	}

	// oldSeen[name] is only ever consulted for presence; replacement[name]
	// only ever needs any one non-empty digest that differs from the old
	// one - a full per-name set of every distinct digest observed is never
	// otherwise used, so it isn't built.
	oldSeen := make(map[string]bool, len(oldDigests))
	replacement := make(map[string]string, len(oldDigests))
	for _, n := range nodes {
		n.mu.RLock()
		for _, m := range n.LoadedModels {
			if m.Name == "" || m.Digest == "" {
				continue
			}
			old, tracked := oldDigests[m.Name]
			if !tracked {
				continue
			}
			if m.Digest == old {
				oldSeen[m.Name] = true
			} else {
				replacement[m.Name] = m.Digest
			}
		}
		n.mu.RUnlock()
	}

	r.digestMu.Lock()
	defer r.digestMu.Unlock()
	for name, newDigest := range replacement {
		if oldSeen[name] {
			continue
		}
		r.modelDigests[name] = newDigest
	}
}

// digestMismatch reports whether digest is known to differ from the
// first-observed digest recorded for name. Always false when either side is
// empty - a runtime that doesn't report a digest (anything but Ollama today,
// per ModelInfo.Digest) or a name with no digest recorded yet is never
// flagged - this never fabricates a comparison from missing data.
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
// under one model name.
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
// modelName at all - a hard eligibility filter applied before scoring.
// Ollama is exempt: its runtime can load/pull a requested model on demand, so
// absence from LoadedModels does not disqualify it (existing cold-start
// behavior, unchanged). Every other runtime has no such on-demand load path
// from the marbor's perspective, so a non-Ollama node must already report
// modelName in LoadedModels - otherwise a healthy-but-wrong-model node could
// silently serve output from a different model than the one requested.
func (r *Router) isEligibleForModel(n *NodeState, modelName string) bool {
	if modelName == "" {
		return true
	}
	// "auto" is a pending-detection state, not an actual runtime - it can
	// legitimately resolve to any of the 5 supported runtimes once pollNode
	// completes detection. Excluding it here would wrongly disqualify a
	// healthy auto-detect node from candidacy for any not-yet-warm model
	// during that window; failing open (like Ollama's own on-demand-load
	// exemption) is correct since the runtime isn't known yet, not because
	// this assumes any particular one of the 5.
	if runtime := n.GetRuntime(); runtime == "" || runtime == "ollama" || runtime == "auto" {
		return true
	}
	return r.isModelWarm(n, modelName)
}

// isUnderCapacity reports whether n may be a routing candidate at all under
// the per-node in-flight cap - an eligibility filter based on capacity limits.
// A node at or over its effective cap is excluded from
// candidacy this call - never queued - so the existing RouteExcluding/retry/
// cloud-fallback chain in proxy.go picks the next candidate instead.
//
// Effective cap resolution: NodeState.MaxInFlight (per-node override, set via
// PatchNode/node_overrides) wins if > 0; otherwise Router.maxInFlightPerNode
// (the global RoutingConfig.MaxInFlightPerNode default) applies. An effective
// cap <= 0 means uncapped, matching how
// QueueMaxDepth/QueueTimeoutMs treat 0 as "disabled" elsewhere in this package.
//
// NOT an atomic reservation: this reads ActiveConns at routing-decision time,
// but the matching increment (IncrConn) only happens later in proxy.go once a
// node is actually chosen - there is no reserve-then-commit step like
// reserveColdStartBytes uses for VRAM. A burst of N concurrent requests that
// all evaluate this check before any of their IncrConn calls land can all
// select the same node, overshooting the cap by up to N. This is a best-
// effort/approximate cap, not a hard atomic concurrency guarantee - closing
// that gap would need per-node reservation accounting, deliberately out of
// scope for the per-node in-flight cap feature.
func (r *Router) isUnderCapacity(n *NodeState) bool {
	n.mu.RLock()
	effectiveCap := n.MaxInFlight
	n.mu.RUnlock()
	if effectiveCap <= 0 {
		effectiveCap = r.maxInFlightPerNode
	}
	if effectiveCap <= 0 {
		return true
	}
	return atomic.LoadInt32(&n.ActiveConns) < int32(effectiveCap)
}

// isGPUGroupSufficient reports whether n's effective GPU group can satisfy its
// derived placement requirement. A node with required==0 (no parallelism
// declared and no gpu_indices) is always sufficient - existing fleet unaffected.
// When AgentGPUs is unknown (avail 0), fail open (true) rather than failing
// closed and 503 the fleet on agent outage - same as the gpuCountUnknown fail-open behavior.
func (r *Router) isGPUGroupSufficient(n *NodeState) bool {
	req := n.EffectiveRequiredGPUs()
	if req == 0 {
		return true
	}
	avail := r.effectiveAvailableGPUs(n)
	if avail == 0 {
		return true
	}
	return avail >= req
}

func (r *Router) effectiveAvailableGPUs(n *NodeState) int {
	n.mu.RLock()
	agentGPUs := append([]marboragent.GPUInfo(nil), n.AgentGPUs...)
	declared := append([]int(nil), n.DeclaredGPUIndices...)
	detected := append([]int(nil), n.DetectedGPUGroup...)
	n.mu.RUnlock()
	// Declared wins for scoping when present (operator override).
	if len(declared) > 0 {
		if len(agentGPUs) == 0 {
			return len(declared)
		}
		scoped, applied := scopeGPUsForPlacement(agentGPUs, declared)
		if !applied {
			return len(agentGPUs)
		}
		return len(scoped)
	}
	// No declared - fallback to detected group for honest per-runtime
	// scoping when a host runs two runtimes on different GPU subsets via
	// CUDA_VISIBLE_DEVICES (e.g. 0..3 vs 4..7). If no detected group, fall
	// back to host inventory count (original behavior, fail-open).
	if len(detected) > 0 {
		if len(agentGPUs) == 0 {
			return len(detected)
		}
		scoped, applied := scopeGPUsForPlacement(agentGPUs, detected)
		if !applied {
			return len(agentGPUs)
		}
		return len(scoped)
	}
	if len(agentGPUs) > 0 {
		return len(agentGPUs)
	}
	return 0
}

// scopeGPUsForPlacement mirrors internal/admin/catalog scopeGPUsToDeclared but
// lives in router to avoid import cycle - same semantics: declared empty no-op,
// no match fallback to unscoped.
func scopeGPUsForPlacement(agentGPUs []marboragent.GPUInfo, declaredIndices []int) (scoped []marboragent.GPUInfo, applied bool) {
	if len(declaredIndices) == 0 {
		return agentGPUs, false
	}
	want := make(map[int]bool, len(declaredIndices))
	for _, idx := range declaredIndices {
		want[idx] = true
	}
	out := make([]marboragent.GPUInfo, 0, len(agentGPUs))
	for _, g := range agentGPUs {
		if want[g.Index] {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return agentGPUs, false
	}
	return out, true
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
// KV-cache round-trip on the next request for each of them. Called on the same periodic cadence
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
// so sticky sessions survive a marbor restart. Entries already past the TTL
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
// "fall through to normal routing." The second return value reports whether
// an affinity entry existed for sessionID at all (regardless of whether it
// turned out valid) - used by Route to distinguish "no affinity requested"
// from "affinity requested but expired/unhealthy" for RoutingDecision.
func (r *Router) stickyNode(sessionID string) (*NodeState, bool) {
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
	if !ok {
		return nil, false
	}
	if time.Since(time.Unix(0, lastSeenNano)) >= r.affinityTTL {
		return nil, true
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
		// Delete only if the map still holds the exact entry we read above
		// (pointer identity) - not the old dead TTL guard (line 296's early
		// return already guarantees the entry was within TTL when read, so
		// that guard could never be true), but a real guard is still needed:
		// a concurrent Route() call for the same sessionID can replace this
		// entry with a brand-new *affinityEntry (a freshly established pin
		// to a different, now-healthy node) between our RUnlock above and
		// this Lock - deleting unconditionally would erase that fresh pin
		// instead of the stale one we actually looked up.
		r.affinityMu.Lock()
		if e, ok := r.affinity[sessionID]; ok && e == entry {
			delete(r.affinity, sessionID)
		}
		r.affinityMu.Unlock()
		return nil, true
	}
	sticky.mu.RLock()
	healthy := sticky.Healthy
	draining := sticky.Draining
	sticky.mu.RUnlock()
	if !healthy || draining {
		// Same pointer-identity guard as the sticky==nil branch above.
		r.affinityMu.Lock()
		if e, ok := r.affinity[sessionID]; ok && e == entry {
			delete(r.affinity, sessionID)
		}
		r.affinityMu.Unlock()
		return nil, true
	}

	r.affinityMu.RLock()
	if e, ok := r.affinity[sessionID]; ok {
		e.lastSeen.Store(time.Now().UnixNano())
	}
	r.affinityMu.RUnlock()
	return sticky, true
}

// staticVRAMReservation reports whether runtime statically pre-allocates
// GPU memory at startup (weights + KV cache), making VRAMUsedMB a
// by-design constant rather than a live pressure signal. Confirmed
// today for vLLM only (gpu-memory-utilization); TGI has a static mode too
// but that is not yet confirmed against this marbor's telemetry, so it is
// deliberately NOT included here - narrowing to what's verified, not
// guessing across the runtime matrix. Ollama,
// llama.cpp, and MLX dynamically allocate and are unaffected.
func staticVRAMReservation(runtime string) bool {
	return runtime == "vllm"
}

// effectiveLoad weights a node's raw active-connection count by its recent
// observed load shape: Orca-style continuous batching means every
// connection is not equal work - a prefill-heavy request keeps the node's
// shared compute busy far longer before its first byte than a decode-light
// continuation, so counting both as one "slot" (the old inverse_queue_depth
// arithmetic) makes a node serving a few heavy requests look less loaded
// than one serving many light ones. n.RecentTTFT (real observed
// time-to-first-byte, RecordTTFT) is the cheapest available proxy for this:
// it degrades when a node's connections are actually compute-bound, and does
// so without inventing a prefill/decode phase classifier the router has no
// visibility into. A node with no TTFT history yet (new/idle) falls back to
// the raw connection count unchanged - this is a refinement of the existing
// formula, not a new code path that can diverge from it when data is
// missing - this never fabricates a load signal from nothing.
//
// Must be called with n.mu already held (RLock is sufficient) - it reads
// n.RecentTTFT directly, matching every other per-node field scoreComponents
// reads under its own RLock.
func effectiveLoad(n *NodeState, conns int32) float64 {
	if len(n.RecentTTFT) == 0 {
		return float64(conns)
	}
	sum := 0.0
	for _, ttft := range n.RecentTTFT {
		sum += ttft
	}
	avgTTFT := sum / float64(len(n.RecentTTFT))
	return float64(conns) * (1.0 + avgTTFT)
}

// scoreComponents calculates the multi-factor score breakdown for a node,
// term by term, in the exact order and arithmetic computeNodeScore has
// always used:
// score = (warm_model_resident * 50) + (free_vram_headroom * 20) +
//
//	(inverse_queue_depth * 15) + (node_health_score * 10) +
//	(recent_success_rate * 5), then cooldown and stale-telemetry
//	penalties applied in sequence, each floored at 0.
//
// This is the single source of truth for the scoring arithmetic -
// computeNodeScore sums the returned Values rather than recomputing the
// score, so a caller building a RoutingDecision from this breakdown is
// guaranteed to see the exact number the router used to pick the winner.
func (r *Router) scoreComponents(n *NodeState, model string) []ScoreComponent {
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
		// serve. Instantaneous free bytes is not a capacity signal for
		// these runtimes - do not credit or penalize the reserved block.
		// Use the same real, already-tracked in-flight request count that
		// drives factor 3 (inverse_queue_depth) as the "capacity to accept
		// another request" signal instead; no runtime today reports a
		// usable queue-depth or declared-concurrency telemetry field
		// (marboragent.RuntimeInfo.QueueDepth is never populated).
		conns := atomic.LoadInt32(&n.ActiveConns)
		freeVRAM = 1.0 / (1.0 + effectiveLoad(n, conns))
	} else if n.VRAMTotalMB > 0 {
		// FragmentationOverheadMult: allocator/PagedAttention block slack and
		// CUDA graph bookkeeping consume real VRAM beyond the sum of loaded
		// models' reported bytes (see eviction.go's EvictForHeadroom for the
		// full rationale) - discount it here too so this scoring factor and
		// the eviction/headroom decisions agree on what "free" means.
		free := n.VRAMTotalMB - int64(float64(n.VRAMUsedMB)*FragmentationOverheadMult)
		// Discount VRAM already reserved-but-unconfirmed by an in-flight
		// cold-start pick on this node (see reserveColdStartBytes) - without
		// this, two concurrent requests for two different cold models can
		// both see the same stale last-poll snapshot and both pick this node
		// before either load is confirmed by the next poll.
		if reserved := r.PendingPrewarmBytes(n.Name); reserved > 0 {
			free -= reserved / (1024 * 1024)
		}
		if free > 0 {
			freeVRAM = float64(free) / float64(n.VRAMTotalMB)
		}
	}

	// 3. inverse_queue_depth
	conns := atomic.LoadInt32(&n.ActiveConns)
	invQueue := 1.0 / (1.0 + effectiveLoad(n, conns))

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

	components := []ScoreComponent{
		{Name: "warm_model_resident", Raw: warm, Weight: 50.0, Value: warm * 50.0},
		{Name: "free_vram_headroom", Raw: freeVRAM, Weight: 20.0, Value: freeVRAM * 20.0},
		{Name: "inverse_queue_depth", Raw: invQueue, Weight: 15.0, Value: invQueue * 15.0},
		{Name: "node_health", Raw: health, Weight: 10.0, Value: health * 10.0},
		{Name: "success_rate", Raw: success, Weight: 5.0, Value: success * 5.0},
	}
	running := sumComponents(components)

	// Cooldown penalty: reduce node score by 50 points if in 60s cooldown,
	// floored at 0. Value records the actual delta applied, which is less
	// than -50 if the floor already cut it short.
	cooldownTriggered := !n.LastErrorAt.IsZero() && time.Since(n.LastErrorAt) < 60*time.Second
	cooldownValue := 0.0
	if cooldownTriggered {
		next := running - 50.0
		if next < 0 {
			next = 0
		}
		cooldownValue = next - running
		running = next
	}
	components = append(components, ScoreComponent{
		Name: "cooldown_penalty", Raw: boolToFloat(cooldownTriggered), Weight: -50.0, Value: cooldownValue,
	})

	// Stale-telemetry penalty: markFailure only flips Healthy false after
	// healthFailureThreshold CONSECUTIVE poll failures (health.go), so a node
	// that just crashed keeps scoring as if its last-known VRAM/queue/loaded-
	// models snapshot were still current for that whole grace window. Once a
	// node's poll data is older than the grace window a healthy node's poll
	// cadence would have refreshed it by, apply the same -50 penalty as the
	// error cooldown above rather than trusting a snapshot that's actually
	// gone stale.
	staleTriggered := false
	if !n.LastPollAt.IsZero() && r.interval > 0 {
		staleAfter := time.Duration(r.healthFailureThreshold) * r.interval
		staleTriggered = time.Since(n.LastPollAt) > staleAfter
	}
	staleValue := 0.0
	if staleTriggered {
		next := running - 50.0
		if next < 0 {
			next = 0
		}
		staleValue = next - running
		running = next
	}
	components = append(components, ScoreComponent{
		Name: "stale_telemetry_penalty", Raw: boolToFloat(staleTriggered), Weight: -50.0, Value: staleValue,
	})

	return components
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

// computeNodeScore calculates a multi-factor score for a node. See
// scoreComponents for the term-by-term breakdown this sums.
func (r *Router) computeNodeScore(n *NodeState, model string) float64 {
	return sumComponents(r.scoreComponents(n, model))
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
// written after every candidate had been scored (selectBestNode),
// so a concurrent call scoring the same node saw stale headroom for the
// entire scoring pass, not just the time after this candidate became the
// leader.
func (r *Router) findBestByScore(nodes []*NodeState, modelName string) (*NodeState, []ScoreComponent) {
	var bestNode *NodeState
	var bestScore float64 = -999.0
	var bestComponents []ScoreComponent
	var reservedFor *NodeState // node currently holding this loop's provisional reservation, if any

	for _, n := range nodes {
		components := r.scoreComponents(n, modelName)
		score := sumComponents(components)
		isNewBest := bestNode == nil || score > bestScore || (score == bestScore && n.Name < bestNode.Name)
		if !isNewBest {
			continue
		}
		bestNode = n
		bestScore = score
		bestComponents = components

		if reservedFor != nil && reservedFor != n {
			r.clearWarmReservation(reservedFor.Name, modelName)
			reservedFor = nil
		}
		if modelName != "" && !r.isModelWarm(n, modelName) {
			r.reserveColdStartBytes(n.URL, n.Name, modelName)
			reservedFor = n
		}
	}
	return bestNode, bestComponents
}

// selectBestNode runs scoring and handles pinned models.
// If the model is pinned and warm on any healthy candidate, it is selected immediately.
func (r *Router) selectBestNode(candidates []*NodeState, modelName string) (*NodeState, bool, *RoutingDecision) {
	if len(candidates) == 0 {
		return nil, false, nil
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
			bestNode, _ := r.findBestByScore(pinnedAndWarm, modelName)
			metrics.CacheHit()
			decision := &RoutingDecision{Reason: ReasonPinnedWarm}
			if bestNode != nil {
				decision.Node = bestNode.Name
				decision.Detail = "pinned+warm on node " + bestNode.Name
			}
			return bestNode, true, decision
		}
	}

	// 2. Score all candidates
	bestNode, components := r.findBestByScore(candidates, modelName)
	if bestNode == nil {
		return nil, false, nil
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
	decision := &RoutingDecision{
		Node:       bestNode.Name,
		Reason:     ReasonScoreBased,
		Detail:     "score_based on node " + bestNode.Name,
		Score:      sumComponents(components),
		Components: components,
	}
	return bestNode, warm, decision
}

// routeInternal is the core weighted selection logic that Route delegates to.
func (r *Router) routeInternal(modelName, runtimeFilter string) (*NodeState, bool, *RoutingDecision) {
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
		if isHealthy && !isDraining && r.isEligibleForModel(n, modelName) && r.isUnderCapacity(n) && r.isGPUGroupSufficient(n) {
			healthy = append(healthy, n)
		}
	}
	return r.selectBestNode(healthy, modelName)
}

// Route picks the best healthy node for modelName using weighted placement scoring.
// If sessionID is non-empty and a valid affinity entry exists for it, the
// previously-used node is preferred (sticky session). The returned
// RoutingDecision explains why the node was picked; AffinityLost is
// set when a session had an affinity entry that existed but did not
// validate (expired, target unhealthy/draining/ineligible), so the eventual
// score_based/pinned_warm decision doesn't silently look like a request that
// never had affinity at all.
func (r *Router) Route(modelName, sessionID, runtimeFilter string) (*NodeState, bool, *RoutingDecision) {
	if !r.sessionAffinity {
		sessionID = ""
	}
	affinityLost := false
	if sessionID != "" {
		node, hadEntry := r.stickyNode(sessionID)
		if node != nil {
			hardValid := (runtimeFilter == "" || node.GetRuntime() == runtimeFilter) && r.isEligibleForModel(node, modelName) && r.isGPUGroupSufficient(node)
			if hardValid && r.isUnderCapacity(node) {
				r.RecordTransition(modelName, time.Now())
				warm := r.isModelWarm(node, modelName)
				if !warm {
					// Same race guard as selectBestNode's cold-start path -
					// the sticky-session shortcut bypasses selectBestNode
					// entirely, so it needs its own reservation write.
					r.reserveColdStartBytes(node.URL, node.Name, modelName)
				}
				decision := &RoutingDecision{
					Node:   node.Name,
					Reason: ReasonSessionAffinity,
					Detail: "sticky to node " + node.Name,
				}
				return node, warm, decision
			}
			if !hardValid {
				r.affinityMu.Lock()
				delete(r.affinity, sessionID)
				r.affinityMu.Unlock()
			}
			affinityLost = true
		} else if hadEntry {
			affinityLost = true
		}
	}

	node, warm, decision := r.routeInternal(modelName, runtimeFilter)
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
		if decision != nil && affinityLost {
			decision.AffinityLost = true
			decision.Detail += " (session affinity existed but target node unhealthy/draining/expired)"
		}
	}
	return node, warm, decision
}

// RouteExcluding picks the best healthy node for modelName using weighted placement scoring,
// excluding any node whose URL appears in the exclude map. It never reads or
// writes session affinity, so it carries no AffinityLost signal - callers
// retrying after a failure annotate that context onto the returned
// RoutingDecision.Detail themselves (the router stays ignorant of retry
// semantics by design).
func (r *Router) RouteExcluding(modelName, runtimeFilter string, exclude map[string]bool) (*NodeState, bool, *RoutingDecision) {
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
		if isHealthy && !isDraining && r.isEligibleForModel(n, modelName) && r.isUnderCapacity(n) && r.isGPUGroupSufficient(n) {
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
