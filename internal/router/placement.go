package router

// placement.go - Placement and routing-decision logic.
//
// Contains the core weighted scoring and selection logic that Route and
// RouteExcluding delegate to. Extracted from router.go; supports
// multi-factor placement scoring, model pinning, and node cooldown.

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
		{Name: "warm_model_resident", Raw: warm, Weight: 50.0, Value: warm * 50.0, Phase: PhaseLocality},
		{Name: "free_vram_headroom", Raw: freeVRAM, Weight: 20.0, Value: freeVRAM * 20.0, Phase: PhaseLocality},
		{Name: "inverse_queue_depth", Raw: invQueue, Weight: 15.0, Value: invQueue * 15.0, Phase: PhasePredictedPerformance},
		{Name: "node_health", Raw: health, Weight: 10.0, Value: health * 10.0, Phase: PhaseReliability},
		{Name: "success_rate", Raw: success, Weight: 5.0, Value: success * 5.0, Phase: PhaseReliability},
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
		Name: "cooldown_penalty", Raw: boolToFloat(cooldownTriggered), Weight: -50.0, Value: cooldownValue, Phase: PhaseReliability,
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
		Name: "stale_telemetry_penalty", Raw: boolToFloat(staleTriggered), Weight: -50.0, Value: staleValue, Phase: PhaseReliability,
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
		// No candidate survived the pre-score hard filter. A non-nil decision
		// is still returned (Node empty) so applyExclusionExplainability has
		// somewhere to attach Excluded/ExcludedTotal - this is the scenario
		// an operator most needs that data for.
		return nil, false, &RoutingDecision{Reason: ReasonNoCandidate}
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

// --- Multi-host TP/PP replica scheduling ---
//
// A Replica is a computed view over one or more NodeState values - never its
// own persisted table (the only persisted fact is node_overrides.replica_peers,
// read into NodeState.ReplicaPeers). SchedulingRole is a topology label,
// orthogonal to health/draining: RoleWorker/RoleUnresolved mean "this node is
// never a direct placement target," nothing about whether the node itself is
// healthy.

// ReplicaMemberScope pins a GPU index list to the specific member it was read
// from. GPU indices are only meaningful relative to the host that reported
// them - index 0 on host A and index 0 on host B are different physical
// devices - so this is the only shape GPU scope is allowed to take anywhere
// in the Replica view. Never flattened to a bare []int.
type ReplicaMemberScope struct {
	Node *NodeState
	GPUs []int
}

// Replica is a computed, request-time view over one or more NodeState
// values participating in one schedulable deployment - never persisted as
// its own row. Members/Head/GPUScopes/ParallelismType/Width/ModelVariant/
// Capabilities are all derived from the member NodeStates' own existing
// fields; replica_peers never carries a duplicate copy of any of them.
type Replica struct {
	Members          []*NodeState         // 1 for standalone; >1 for a multi-host TP/PP group
	Head             *NodeState           // operator-declared - the node clients route to
	GPUScopes        []ReplicaMemberScope // one entry per member; never compared/merged across members by index
	ParallelismType  string               // standalone: that member's own value. multi-member: validated consistent across members
	ParallelismWidth int                  // standalone: that member's own value. multi-member: the topology's total shape
	ModelVariant     string               // validated consistent across members
	Capabilities     []string             // the served model's own capability set (chat/embedding/vision/tools)
}

// SchedulingRole classifies a node's place in its (possibly single-member)
// replica. It is a topology label, computed fresh every call from
// replica_peers - not a health state, and has no "undo": a worker doesn't
// stop being a worker by becoming healthy again.
type SchedulingRole int

const (
	// RoleStandalone is the zero value: not part of any declared multi-host
	// replica. Every node in today's existing fleets resolves here.
	RoleStandalone SchedulingRole = iota
	// RoleHead is the node clients/marbor route requests to - the only
	// member of a multi-host replica ever selected as a placement target.
	RoleHead
	// RoleWorker is a confirmed non-head member of a valid multi-host
	// replica. Never directly schedulable, regardless of its own health.
	RoleWorker
	// RoleUnresolved means this node is named in at least one replica_peers
	// declaration, but the declarations disagree (asymmetric, conflicting
	// members, conflicting head, or a declaration naming a nonexistent
	// node) - excluded from placement identically to RoleWorker, never
	// silently downgraded to RoleStandalone and never guessed into
	// RoleWorker/RoleHead, until the operator reconciles the declarations.
	RoleUnresolved
)

// replicaDecl is one node's own replica_peers declaration, normalized to a
// set for order-independent membership comparison.
type replicaDecl struct {
	members map[string]bool
	head    string
}

// resolveSchedulingRoles computes every node's SchedulingRole in one pass
// over the given node list, as a graph-connectivity problem:
//  1. Every node that declares replica_peers draws an edge to each of its
//     declared members (whether or not that member exists in the fleet, or
//     declares anything back - the edge exists because it was NAMED).
//  2. Union-find (disjoint-set) groups every name transitively reachable
//     through those edges into one connected component - this is what makes
//     the result immune to node iteration order: union-find's output
//     depends only on which edges exist, never the order they were added or
//     the order components are later visited.
//  3. Each component is validated ONCE, as a whole: every member of the
//     component must be a real node in the fleet, must itself declare
//     replica_peers, and that declaration's member set and head must be
//     identical to every other member's. Any single mismatch invalidates
//     the ENTIRE component - not just the node whose declaration triggered
//     the check. This is deliberate: a node's own declaration alone never
//     proves it is safe to schedule - the whole component it belongs to
//     must be valid, because it can be pulled into an invalid component by
//     another member's bad declaration.
//  4. Every real node in a valid component becomes Head (if it is the
//     agreed head) or Worker. Every real node in an invalid component
//     becomes RoleUnresolved.
//  5. A node touched by no declaration (never named, names nothing) is in
//     no component at all and is absent from the returned map - callers
//     treat absence as RoleStandalone.
//
// Computed once per routing call, not per candidate. Cost is linear in the
// number of declared-replica_peers edges, trivial at 4-20-node fleet scale.
// Returns a map keyed by node name. Never mutates any NodeState.
func (r *Router) resolveSchedulingRoles(nodes []*NodeState) map[string]SchedulingRole {
	// nodeSet is every REAL node in this fleet snapshot - used to fail
	// closed on a declaration naming a node that doesn't actually exist.
	nodeSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.Name] = true
	}

	declBy := make(map[string]replicaDecl) // name -> that node's OWN declaration, only for names that declared one

	// Union-find over every name that appears anywhere in any declaration
	// (as declarer or as a named member) - deliberately not limited to
	// names in nodeSet, so a reference to a nonexistent node still joins
	// its component instead of silently vanishing.
	parent := make(map[string]string)
	var find func(string) string
	find = func(x string) string {
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for _, n := range nodes {
		n.mu.RLock()
		rp := n.ReplicaPeers
		n.mu.RUnlock()
		if rp == nil || len(rp.Members) == 0 {
			continue
		}
		memberSet := make(map[string]bool, len(rp.Members))
		for _, m := range rp.Members {
			memberSet[m] = true
			union(n.Name, m) // edge: n <-> every member it names, regardless of whether m exists or declares back
		}
		declBy[n.Name] = replicaDecl{members: memberSet, head: rp.Head}
	}

	if len(declBy) == 0 {
		return nil // fast path: nobody in this fleet has declared anything
	}

	// Group every name touched by union-find into its component, keyed by
	// that component's root. Order-independent: iterates union-find's own
	// internal state, not the caller's nodes slice order.
	components := make(map[string]map[string]bool) // root -> {every name in that component}
	for name := range parent {
		root := find(name)
		if components[root] == nil {
			components[root] = make(map[string]bool)
		}
		components[root][name] = true
	}

	roles := make(map[string]SchedulingRole, len(nodes))
	for _, members := range components {
		valid, head := validateComponent(members, nodeSet, declBy)
		for name := range members {
			if !nodeSet[name] {
				continue // a name referenced by a declaration but not a real node - nothing to assign a role to
			}
			if !valid {
				roles[name] = RoleUnresolved
				continue
			}
			if name == head {
				roles[name] = RoleHead
			} else {
				roles[name] = RoleWorker
			}
		}
	}
	return roles
}

// validateComponent reports whether every real node inside members has an
// identical, reciprocal replica_peers declaration naming EXACTLY this
// component's members (itself included) and the same head - the concrete
// form of the fail-closed union/closure rule, decided once for the whole
// component rather than accumulated per node. Returns the agreed head only
// when valid is true.
func validateComponent(members map[string]bool, nodeSet map[string]bool, declBy map[string]replicaDecl) (valid bool, head string) {
	var agreedHead string
	headSeen := false
	for name := range members {
		if !nodeSet[name] {
			return false, "" // component references a node that does not exist in this fleet - fail closed
		}
		d, ok := declBy[name]
		if !ok {
			return false, "" // this member is IN the component (something else named it) but never declared back - one-sided
		}
		if len(d.members) != len(members) {
			return false, "" // this member's declared set is a different size than the component - can't be identical
		}
		for m := range members {
			if !d.members[m] {
				return false, "" // this member's declared set doesn't include every name in the component (itself included)
			}
		}
		if !headSeen {
			agreedHead = d.head
			headSeen = true
		} else if d.head != agreedHead {
			return false, "" // conflicting head between two members
		}
	}
	if agreedHead == "" || !members[agreedHead] {
		return false, "" // head must be declared and must be one of the component's own members
	}
	return true, agreedHead
}

// componentFor returns the full member NodeState list and agreed head name
// for n's connected component, recomputing the same union-find/declBy state
// resolveSchedulingRoles uses (resolveSchedulingRoles itself only returns
// per-name roles, not the member list). Only meaningful when n's role is
// RoleHead or RoleWorker; callers must check that first.
func componentFor(name string, allNodes []*NodeState) (members []*NodeState, head string) {
	byName := make(map[string]*NodeState, len(allNodes))
	nodeSet := make(map[string]bool, len(allNodes))
	for _, n := range allNodes {
		byName[n.Name] = n
		nodeSet[n.Name] = true
	}

	declBy := make(map[string]replicaDecl)
	parent := make(map[string]string)
	var find func(string) string
	find = func(x string) string {
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, n := range allNodes {
		n.mu.RLock()
		rp := n.ReplicaPeers
		n.mu.RUnlock()
		if rp == nil || len(rp.Members) == 0 {
			continue
		}
		memberSet := make(map[string]bool, len(rp.Members))
		for _, m := range rp.Members {
			memberSet[m] = true
			union(n.Name, m)
		}
		declBy[n.Name] = replicaDecl{members: memberSet, head: rp.Head}
	}

	if _, ok := parent[name]; !ok {
		return []*NodeState{byName[name]}, ""
	}
	root := find(name)
	var memberNames []string
	for nm := range parent {
		if find(nm) == root {
			memberNames = append(memberNames, nm)
		}
	}
	memberSet := make(map[string]bool, len(memberNames))
	for _, nm := range memberNames {
		memberSet[nm] = true
	}
	_, agreedHead := validateComponent(memberSet, nodeSet, declBy)
	for _, nm := range memberNames {
		if n, ok := byName[nm]; ok {
			members = append(members, n)
		}
	}
	return members, agreedHead
}

// effectiveGPUIndicesLocked returns n's effective GPU scope (declared,
// falling back to detected) - same declared-wins-over-detected precedence
// effectiveRequiredGPUsLocked already uses. Caller must hold n.mu (read or
// write lock).
func effectiveGPUIndicesLocked(n *NodeState) []int {
	if len(n.DeclaredGPUIndices) > 0 {
		return n.DeclaredGPUIndices
	}
	return n.DetectedGPUGroup
}

// primaryLoadedModelVariantLocked returns a simple content-identity string
// for n's currently-loaded model set (name@digest of the first loaded
// model), or "" if none is loaded. Caller must hold n.mu.
func primaryLoadedModelVariantLocked(n *NodeState) string {
	if len(n.LoadedModels) == 0 {
		return ""
	}
	m := n.LoadedModels[0]
	if m.Digest == "" {
		return m.Name
	}
	return m.Name + "@" + m.Digest
}

// allMembersAgreeOnModel reports whether every member of a multi-host
// replica resolves to the same primaryLoadedModelVariantLocked value. An
// empty (nothing loaded yet) value on every member still counts as
// agreement; a mix of empty and non-empty, or two different non-empty
// values, does not.
func allMembersAgreeOnModel(members []*NodeState) bool {
	if len(members) == 0 {
		return false
	}
	var first string
	for i, m := range members {
		m.mu.RLock()
		v := primaryLoadedModelVariantLocked(m)
		m.mu.RUnlock()
		if i == 0 {
			first = v
			continue
		}
		if v != first {
			return false
		}
	}
	return true
}

// allMembersCompatibleParallelism reports whether every member declares the
// same parallelism type. Width-arithmetic validation ("do the declared
// widths describe one coherent total topology") is left to a future
// consumer to refine once a real multi-host deployment's exact declaration
// convention is observed in the field - this function only checks the
// invariant that is unambiguous today: every member must agree on TYPE.
func allMembersCompatibleParallelism(members []*NodeState) bool {
	if len(members) == 0 {
		return false
	}
	var first string
	for i, m := range members {
		m.mu.RLock()
		t := m.ParallelismType
		m.mu.RUnlock()
		if i == 0 {
			first = t
			continue
		}
		if t != first {
			return false
		}
	}
	return first != ""
}

// resolvedTopologyShape returns the multi-member replica's ParallelismType
// (agreed across members, per allMembersCompatibleParallelism) and
// ParallelismWidth (the topology's total width - sum of each member's own
// declared width, the natural reading for a TP/PP deployment sharded across
// hosts where each member declares its own local share).
func resolvedTopologyShape(members []*NodeState) (string, int) {
	var t string
	total := 0
	for _, m := range members {
		m.mu.RLock()
		if t == "" {
			t = m.ParallelismType
		}
		total += m.ParallelismWidth
		m.mu.RUnlock()
	}
	return t, total
}

// capabilitiesForVariant returns the served model's own capability set for
// a validated model variant identity. No Model Advisor capability-metadata
// lookup is wired into this package yet, so this deliberately returns nil
// (never fabricated) - a future consumer (e.g. P422) wires the real lookup
// in without changing replicaFor's contract. Empty when variant is empty
// (unresolved model identity), never guessed from one member.
func capabilitiesForVariant(variant string) []string {
	if variant == "" {
		return nil
	}
	return nil
}

// replicaFor resolves n's full Replica view, built on the same closure
// computation resolveSchedulingRoles uses - it never re-derives role
// resolution independently, so the two can never disagree about a node's
// role.
//
// Binding contract guardrail: a zero-valued field must never be
// interpretable as a valid replica configuration. On a STANDALONE
// (len(Members) == 1) NodeState, ParallelismType == "" / ParallelismWidth
// == 0 legitimately means "unconstrained" - the existing, pre-replica
// meaning of the zero value everywhere else in this codebase. On a
// MULTI-MEMBER (len(Members) > 1) Replica returned here for a
// RoleHead/RoleWorker node, those same zero values mean something
// completely different - conflicting, validation failed, unknown - and
// must never be read as "unconstrained." The distinction is by
// len(Members), never by inspecting the value itself. SchedulingRole
// (never any Replica field) is the sole authoritative schedulability
// signal: filterCandidates never reads any Replica field, only
// resolveSchedulingRoles's per-name role map, so no live routing decision
// in this package is ever made from a Replica's zero-valued field. Any
// future caller reading Replica.ModelVariant/.ParallelismType/.Width from a
// multi-member Replica MUST treat an empty/zero value as "validation
// failed for this replica - do not use this value for any capacity,
// placement, or advisory decision," never as "unconstrained."
func (r *Router) replicaFor(n *NodeState, allNodes []*NodeState) Replica {
	roles := r.resolveSchedulingRoles(allNodes)
	role := roles[n.Name] // zero value RoleStandalone if absent

	if role == RoleStandalone {
		n.mu.RLock()
		defer n.mu.RUnlock()
		return Replica{
			Members:          []*NodeState{n},
			Head:             n,
			GPUScopes:        []ReplicaMemberScope{{Node: n, GPUs: effectiveGPUIndicesLocked(n)}},
			ParallelismType:  n.ParallelismType,
			ParallelismWidth: n.ParallelismWidth,
			ModelVariant:     primaryLoadedModelVariantLocked(n),
			Capabilities:     capabilitiesForVariant(primaryLoadedModelVariantLocked(n)),
		}
	}

	if role == RoleUnresolved {
		// No agreed member set or head to report, by definition
		// (validateComponent already said so). Informational/debugging
		// only - no live code path in this package calls replicaFor for a
		// RoleUnresolved node for a placement decision; filterCandidates
		// already excludes it before any Replica is ever assembled.
		return Replica{Members: []*NodeState{n}, Head: nil}
	}

	// role is RoleHead or RoleWorker: a valid, agreed component exists.
	members, headName := componentFor(n.Name, allNodes)

	var headNode *NodeState
	for _, m := range members {
		if m.Name == headName {
			headNode = m
			break
		}
	}

	// Model-identity validation: a mismatch does NOT change SchedulingRole
	// (role is a topology label; this is a serving-state conflict) -
	// ModelVariant is left at the zero value ("") rather than silently
	// adopting the head's value, per the guardrail above.
	modelVariant := ""
	if headNode != nil && allMembersAgreeOnModel(members) {
		headNode.mu.RLock()
		modelVariant = primaryLoadedModelVariantLocked(headNode)
		headNode.mu.RUnlock()
	}

	// Parallelism-compatibility validation: on disagreement, left at zero
	// values, never the head's value adopted unanimously.
	parallelismType, parallelismWidth := "", 0
	if allMembersCompatibleParallelism(members) {
		parallelismType, parallelismWidth = resolvedTopologyShape(members)
	}

	gpuScopes := make([]ReplicaMemberScope, 0, len(members))
	for _, m := range members {
		m.mu.RLock()
		gpuScopes = append(gpuScopes, ReplicaMemberScope{Node: m, GPUs: effectiveGPUIndicesLocked(m)})
		m.mu.RUnlock()
	}

	return Replica{
		Members:          members,
		Head:             headNode,
		GPUScopes:        gpuScopes, // never flattened across members
		ParallelismType:  parallelismType,
		ParallelismWidth: parallelismWidth,
		ModelVariant:     modelVariant,
		Capabilities:     capabilitiesForVariant(modelVariant),
	}
}

// filterCandidates applies the pre-score hard filter (runtime match, health,
// draining, model eligibility, per-node capacity, GPU-group shape) that
// routeInternal and RouteExcluding both need, recording which single
// condition eliminated each excluded node - the first one that fails, in the
// exact order the original boolean short-circuit already evaluated:
// runtime filter -> health -> draining -> model eligibility -> capacity ->
// GPU group -> replica worker/unresolved. exclude may be nil (routeInternal
// has no retry-exclude set); a node skipped via exclude is a caller-directed
// retry skip, not a hard-filter exclusion, so it is not recorded as an
// ExcludedCandidate.
func (r *Router) filterCandidates(nodes []*NodeState, modelName, runtimeFilter string, exclude map[string]bool) (healthy []*NodeState, excluded []ExcludedCandidate, excludedTotal int) {
	roles := r.resolveSchedulingRoles(nodes) // computed once for this call, not per node
	for _, n := range nodes {
		if exclude[n.URL] { // safe on a nil map: indexing returns the zero value, never panics
			continue
		}
		reason := ""
		switch {
		case runtimeFilter != "" && n.GetRuntime() != runtimeFilter:
			reason = ExcludeReasonRuntimeMismatch
		default:
			n.mu.RLock()
			isHealthy := n.Healthy
			isDraining := n.Draining
			n.mu.RUnlock()
			switch {
			case !isHealthy:
				reason = ExcludeReasonUnhealthy
			case isDraining:
				reason = ExcludeReasonDraining
			case !r.isEligibleForModel(n, modelName):
				reason = ExcludeReasonIneligibleModel
			case !r.isUnderCapacity(n):
				reason = ExcludeReasonOverCapacity
			case !r.isGPUGroupSufficient(n):
				reason = ExcludeReasonInsufficientGPUGroup
			// Appended after every existing hard-filter case, not inserted
			// earlier - preserves existing ExcludeReason* precedence for a
			// node that fails more than one check. Still absolute hard
			// constraints; only their position in the "which single reason
			// wins" tiebreak is last.
			case roles[n.Name] == RoleWorker:
				reason = ExcludeReasonReplicaWorker
			case roles[n.Name] == RoleUnresolved:
				reason = ExcludeReasonReplicaUnresolved
			}
		}
		if reason == "" {
			healthy = append(healthy, n)
			continue
		}
		excludedTotal++
		if len(excluded) < maxExcludedCandidates {
			excluded = append(excluded, ExcludedCandidate{Node: n.Name, Reason: reason})
		}
	}
	return healthy, excluded, excludedTotal
}

// applyExclusionExplainability copies excluded/excludedTotal onto decision
// (only if non-nil - findBestByScore cannot return a nil node for a non-empty
// candidates slice, but selectBestNode's defensive nil check on that return
// value is left in place, so a nil decision here remains theoretically
// possible and is still handled as a no-op). ExcludedTotal is only set when
// the cap in filterCandidates actually truncated the list.
func applyExclusionExplainability(decision *RoutingDecision, excluded []ExcludedCandidate, excludedTotal int) {
	if decision == nil {
		return
	}
	decision.Excluded = excluded
	if excludedTotal > len(excluded) {
		decision.ExcludedTotal = excludedTotal
	}
}

// routeInternal is the core weighted selection logic that Route delegates to.
func (r *Router) routeInternal(modelName, runtimeFilter string) (*NodeState, bool, *RoutingDecision) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	healthy, excluded, excludedTotal := r.filterCandidates(nodes, modelName, runtimeFilter, nil)
	node, warm, decision := r.selectBestNode(healthy, modelName)
	applyExclusionExplainability(decision, excluded, excludedTotal)
	return node, warm, decision
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
			role := r.resolveSchedulingRoles(r.Nodes())[node.Name] // one extra call, same cost as one filterCandidates pass
			hardValid := (runtimeFilter == "" || node.GetRuntime() == runtimeFilter) &&
				r.isEligibleForModel(node, modelName) &&
				r.isGPUGroupSufficient(node) &&
				role != RoleWorker && role != RoleUnresolved
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

	healthy, excluded, excludedTotal := r.filterCandidates(nodes, modelName, runtimeFilter, exclude)
	node, warm, decision := r.selectBestNode(healthy, modelName)
	applyExclusionExplainability(decision, excluded, excludedTotal)
	return node, warm, decision
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
