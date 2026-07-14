package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// ErrModelPinned is returned by UnloadModel when the requested model is on the
// node's never-evict (pinned) list. Pinning means "never evict or unload
// without an explicit unpin first"  --  it must be honored on every unload path
// (manual and scheduled), not just the automatic LRU eviction path. Callers
// that want to override this must unpin the model first; there is no
// force-unload bypass.
var ErrModelPinned = errors.New("model is pinned; unpin before unloading")

// modelKey composes the lastUsed map key for a (node, model) pair.
func modelKey(node, model string) string { return node + "\x00" + model }

// RecordModelUse stamps the last-request time for (node, model). Called from the
// proxy on every routed request; this timestamp is what drives LRU eviction
// (the coldest model  --  oldest or never-seen  --  is unloaded first under pressure).
func (r *Router) RecordModelUse(node, model string) {
	if node == "" || model == "" {
		return
	}
	r.lruMu.Lock()
	if r.lastUsed == nil {
		r.lastUsed = make(map[string]time.Time)
	}
	r.lastUsed[modelKey(node, model)] = time.Now()
	r.lruMu.Unlock()
}

// lastUsedAt returns the last-request time for (node, model); the zero time
// (never used) sorts as coldest.
func (r *Router) lastUsedAt(node, model string) time.Time {
	r.lruMu.Lock()
	defer r.lruMu.Unlock()
	return r.lastUsed[modelKey(node, model)]
}

// --- pinned models (never evicted, regardless of pressure) ---

// SetPinnedModels sets the never-evict model set for a node. Empty clears it.
func (r *Router) SetPinnedModels(node string, models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pinned == nil {
		r.pinned = make(map[string]map[string]bool)
	}
	if len(models) == 0 {
		delete(r.pinned, node)
		return
	}
	set := make(map[string]bool, len(models))
	for _, m := range models {
		if m != "" {
			set[m] = true
		}
	}
	r.pinned[node] = set
}

// PinnedModels returns the sorted never-evict model list for a node.
func (r *Router) PinnedModels(node string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.pinned[node]
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (r *Router) isPinned(node, model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pinned[node][model]
}

// unloadModel evicts a model from a node's VRAM immediately via Ollama's
// keep_alive:0 on /api/generate (the inverse of a warmup preload). Only Ollama
// backends support this; others are a no-op. reason is a short tag for the log
// line (e.g. "LRU headroom", "manual", "scheduled") so operators can tell an
// automatic eviction from an operator-triggered one.
func (r *Router) unloadModel(ctx context.Context, n *NodeState, model, reason string) error {
	n.mu.RLock()
	nodeURL, rt := n.URL, n.Runtime
	n.mu.RUnlock()
	if rt != "ollama" && rt != "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"model": model, "keep_alive": 0, "stream": false})
	reqCtx, cancel := context.WithTimeout(ctx, warmupPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, nodeURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := warmupHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("node %s returned %d unloading %q", n.Name, resp.StatusCode, model)
	}
	metrics.ModelEvicted(n.Name)
	// Drop the unloaded model from warm state immediately (Tier 1): a manual,
	// scheduled, or LRU-headroom unload is a residency change that must not wait
	// for the background flush, else a crash could restore an evicted model.
	if st := r.warmStore(); st != nil {
		if err := st.DeleteWarmState(model, n.Name); err != nil {
			log.Printf("warmstate: delete %q on %s after unload: %v", model, n.Name, err)
		}
	}
	log.Printf("unloaded model %q from node %s (%s)", model, n.Name, reason)
	return nil
}

// findNode returns the node with the given name, or nil.
func (r *Router) findNode(name string) *NodeState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// nodeExistsLocked reports whether a node with the given name is currently
// in r.nodes. Caller must already hold r.mu (Lock or RLock).
func (r *Router) nodeExistsLocked(name string) bool {
	for _, n := range r.nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

// UnloadModel unloads a single model from a node's VRAM on operator request
// (keep_alive:0). Returns false if the node is unknown. A no-op unload against a
// model that isn't resident is harmless (Ollama returns success). Returns
// ErrModelPinned without contacting the node if the model is on the node's
// never-evict list  --  pinning blocks manual unload the same as auto-eviction;
// the operator must unpin first.
func (r *Router) UnloadModel(ctx context.Context, nodeName, model string) (bool, error) {
	n := r.findNode(nodeName)
	if n == nil {
		return false, nil
	}
	if r.isPinned(nodeName, model) {
		return true, ErrModelPinned
	}
	return true, r.unloadModel(ctx, n, model, "manual")
}

// UnloadModels unloads several models from a node immediately (used by the
// scheduled "unload"/drain-at-night action). Each unload runs in its own
// goroutine so a slow node can't block the scheduler tick. Unknown nodes and
// non-Ollama backends are skipped. Pinned models are skipped (not unloaded)
// with a log line, same policy as the manual UnloadModel path.
func (r *Router) UnloadModels(ctx context.Context, nodeName string, models []string) {
	n := r.findNode(nodeName)
	if n == nil {
		log.Printf("scheduled unload skipped: node %q not found", nodeName)
		return
	}
	for _, m := range models {
		if m == "" {
			continue
		}
		if r.isPinned(nodeName, m) {
			log.Printf("scheduled unload of %q on %s skipped: %v", m, nodeName, ErrModelPinned)
			continue
		}
		m := m
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[router] panic in goroutine: %v", rec)
				}
			}()
			if err := r.unloadModel(ctx, n, m, "scheduled"); err != nil {
				log.Printf("scheduled unload of %q on %s failed: %v", m, nodeName, err)
			}
		}()
	}
}

// EvictForHeadroom unloads the coldest non-pinned models on nodeName until at
// least neededBytes of VRAM is free, or only pinned models remain (in which case
// it logs the unmet pressure  --  a genuine OOM risk, surfaced rather than hidden).
// Returns the number of models evicted. No-op when the node's total VRAM is
// unknown (nothing to reason about).
func (r *Router) EvictForHeadroom(ctx context.Context, nodeName string, neededBytes int64) int {
	r.mu.RLock()
	var target *NodeState
	for _, n := range r.nodes {
		if n.Name == nodeName {
			target = n
			break
		}
	}
	r.mu.RUnlock()
	if target == nil {
		return 0
	}

	type lm struct {
		name string
		size int64
	}
	target.mu.RLock()
	totalBytes := target.VRAMTotalMB * 1024 * 1024
	var loaded []lm
	var usedBytes int64
	for _, m := range target.LoadedModels {
		loaded = append(loaded, lm{m.Name, m.SizeVRAM})
		usedBytes += m.SizeVRAM
	}
	target.mu.RUnlock()
	if totalBytes <= 0 {
		return 0
	}
	free := totalBytes - usedBytes

	evicted := 0
	for free < neededBytes {
		coldIdx := -1
		var coldTime time.Time
		for i, m := range loaded {
			if r.isPinned(nodeName, m.name) {
				continue
			}
			t := r.lastUsedAt(nodeName, m.name)
			if coldIdx == -1 || t.Before(coldTime) {
				coldIdx, coldTime = i, t
			}
		}
		if coldIdx == -1 {
			log.Printf("headroom: node %s needs %d more free bytes but only pinned models remain; cannot make room", nodeName, neededBytes-free)
			break
		}
		victim := loaded[coldIdx]
		if err := r.unloadModel(ctx, target, victim.name, "LRU headroom"); err != nil {
			log.Printf("headroom: failed to evict %q from %s: %v", victim.name, nodeName, err)
			break
		}
		free += victim.size
		loaded = append(loaded[:coldIdx], loaded[coldIdx+1:]...)
		evicted++
	}
	return evicted
}

// evictCooldown bounds how often auto-eviction runs per node, so a node under
// sustained pressure can't thrash (rapid load/evict oscillation).
const evictCooldown = 15 * time.Second

// estimateModelSizeBytes estimates the VRAM a not-yet-loaded model needs from the
// node's /api/tags on-disk size (a good proxy for GGUF weights). Non-Ollama
// runtimes (vllm, tgi, llamacpp) don't expose /api/tags, so FetchModelTags
// fails or the model is absent from the result; in that case, fall back to the
// operator-declared vram_overrides size for that node+model (R1: an explicit
// operator declaration, not a guess). Returns 0 when the size is unknown by
// either path so callers can decline to evict/warm blindly.
func (r *Router) estimateModelSizeBytes(nodeURL, model string) int64 {
	if tags, err := r.FetchModelTags(nodeURL); err == nil {
		for _, t := range tags {
			if t.Name == model {
				return t.Size
			}
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.URL != nodeURL {
			continue
		}
		n.mu.RLock()
		mb, ok := n.VRAMOverrides[model]
		n.mu.RUnlock()
		if ok && mb > 0 {
			return mb * 1024 * 1024
		}
		break
	}
	return 0
}

// warmReservation records that a warmup load for a (node, model) pair has
// started but isn't yet confirmed resident by the poller.
type warmReservation struct {
	bytes int64
	at    time.Time
}

// warmReservationTTL bounds how long an in-flight warmup reservation can
// influence headroom accounting. It mirrors warmupPingTimeout (the longest a
// cold load is allowed to take) so a reservation naturally decays once the
// load could plausibly be finished, even if nothing explicitly clears it
// (e.g. a one-shot caller like predictive prewarm that never rechecks
// residency for that model).
const warmReservationTTL = warmupPingTimeout

// reserveWarmBytes records that `model` on `node` is about to consume estBytes
// of VRAM and returns the bytes already reserved for OTHER models on the same
// node whose warmup is still in flight. Expired reservations (older than
// warmReservationTTL) are dropped opportunistically. Guarded by evictMu.
//
// This exists because n.LoadedModels only reflects the last /api/ps poll: when
// two models are warmed on the same node close together, the second model's
// headroom check would otherwise see the exact same pre-warmup snapshot as the
// first and conclude  --  wrongly  --  that it has the whole node to itself.
func (r *Router) reserveWarmBytes(node, model string, estBytes int64) int64 {
	r.evictMu.Lock()
	defer r.evictMu.Unlock()
	if r.warmReserved == nil {
		r.warmReserved = make(map[string]map[string]warmReservation)
	}
	byModel := r.warmReserved[node]
	if byModel == nil {
		byModel = make(map[string]warmReservation)
		r.warmReserved[node] = byModel
	}
	now := time.Now()
	var others int64
	for m, res := range byModel {
		if now.Sub(res.at) > warmReservationTTL {
			delete(byModel, m)
			continue
		}
		if m == model {
			continue
		}
		others += res.bytes
	}
	byModel[model] = warmReservation{bytes: estBytes, at: now}
	return others
}

// FallbackChainFor returns the operator-declared, ordered list of alternate
// models to try for model, or nil if none is configured. Opt-in only - a
// model absent from routing.fallback_chains has no substitution behavior.
func (r *Router) FallbackChainFor(model string) []string {
	return r.fallbackChains[model]
}

// ModelFitsAnyHealthyNode reports whether model could fit in free VRAM on at
// least one healthy, non-draining node, using the same real size/headroom
// data (tags-cache size, live VRAM) as predictive prewarm and eviction. If no
// healthy node has both a known VRAM total and a known size for model, there
// is no real data to say it doesn't fit, so this fails open (true) - R1:
// never guess a value that wasn't observed.
func (r *Router) ModelFitsAnyHealthyNode(model string) bool {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	sawKnownSize := false
	for _, n := range nodes {
		n.mu.RLock()
		healthy := n.Healthy && !n.Draining
		freeBytes := (n.VRAMTotalMB - n.VRAMUsedMB) * 1024 * 1024
		nodeURL := n.URL
		vramKnown := n.VRAMTotalMB > 0
		n.mu.RUnlock()
		if !healthy || !vramKnown {
			continue
		}
		size := r.estimateModelSizeBytes(nodeURL, model)
		if size <= 0 {
			continue
		}
		sawKnownSize = true
		if freeBytes >= size {
			return true
		}
	}
	return !sawKnownSize
}

// ModelDownloadedAnyNode reports whether model is already present (per
// /api/tags) on at least one node. Used to restrict quantization fallback
// candidates to alternates that are already downloaded - substitution never
// triggers a fresh multi-GB download on the hot path.
func (r *Router) ModelDownloadedAnyNode(model string) bool {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	for _, n := range nodes {
		n.mu.RLock()
		nodeURL := n.URL
		n.mu.RUnlock()
		tags, err := r.FetchModelTags(nodeURL)
		if err != nil {
			continue
		}
		for _, t := range tags {
			if t.Name == model {
				return true
			}
		}
	}
	return false
}

// PendingPrewarmBytes returns the sum of VRAM bytes reserved for in-flight
// warmups on node that haven't yet been confirmed resident by the poller.
// Backed by the same real warmReserved bookkeeping used for headroom
// accounting (reserveWarmBytes) - never a separate estimate - so it decays
// via warmReservationTTL exactly like the accounting it mirrors.
func (r *Router) PendingPrewarmBytes(node string) int64 {
	r.evictMu.Lock()
	defer r.evictMu.Unlock()
	byModel := r.warmReserved[node]
	if byModel == nil {
		return 0
	}
	now := time.Now()
	var total int64
	for _, res := range byModel {
		if now.Sub(res.at) > warmReservationTTL {
			continue
		}
		total += res.bytes
	}
	return total
}

// clearWarmReservation drops any in-flight VRAM reservation for (node, model).
// Called once the poller confirms the model is actually resident, so a stale
// reservation can't keep double-counting against the now-real usedBytes on
// later headroom checks. Guarded by evictMu.
func (r *Router) clearWarmReservation(node, model string) {
	r.evictMu.Lock()
	if byModel := r.warmReserved[node]; byModel != nil {
		delete(byModel, model)
	}
	r.evictMu.Unlock()
}

// ensureHeadroom makes room on a node before it proactively loads `model`. If the
// model isn't already resident and its estimated size won't fit in free VRAM, it
// evicts the coldest non-pinned models first. It is a no-op when the model is
// already loaded, the size or node capacity is unknown, it already fits, or a
// recent auto-eviction on this node is still within the cooldown (thrash guard).
//
// It runs ONLY on the proactive warm/load path  --  never on the streaming request
// path  --  so it never adds latency to a client request.
func (r *Router) ensureHeadroom(ctx context.Context, n *NodeState, model string) {
	n.mu.RLock()
	nodeURL := n.URL
	nodeName := n.Name
	totalBytes := n.VRAMTotalMB * 1024 * 1024
	var usedBytes int64
	resident := false
	for _, m := range n.LoadedModels {
		usedBytes += m.SizeVRAM
		if m.Name == model {
			resident = true
		}
	}
	n.mu.RUnlock()
	if resident {
		// The poller has confirmed this model is loaded; drop any leftover
		// in-flight reservation so it stops double-counting against the real
		// usedBytes above on a later headroom check for a sibling model.
		r.clearWarmReservation(nodeName, model)
	}
	if resident || totalBytes <= 0 {
		return
	}
	est := r.estimateModelSizeBytes(nodeURL, model)
	if est <= 0 {
		return // unknown size
	}

	// Reserve this model's estimated footprint now, and pick up whatever other
	// models on this node are still mid-warmup (started, not yet poll-confirmed).
	// Without this, warming two models on the same node races: both read the
	// identical pre-warmup snapshot and each independently  --  and wrongly  --
	// concludes it has the entire node's free VRAM to itself.
	reservedByOthers := r.reserveWarmBytes(nodeName, model, est)

	if totalBytes-usedBytes-reservedByOthers >= est {
		return // fits alongside real usage and any other in-flight loads
	}
	// Thrash guard: at most one auto-eviction per node per cooldown window.
	r.evictMu.Lock()
	if r.lastEvictAt == nil {
		r.lastEvictAt = make(map[string]time.Time)
	}
	if last, ok := r.lastEvictAt[nodeName]; ok && time.Since(last) < evictCooldown {
		r.evictMu.Unlock()
		return
	}
	r.lastEvictAt[nodeName] = time.Now()
	r.evictMu.Unlock()

	r.EvictForHeadroom(ctx, nodeName, est+reservedByOthers)
}
