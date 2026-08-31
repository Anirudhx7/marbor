package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
)

// ErrModelPinned is returned by UnloadModel when the requested model is on the
// node's never-evict (pinned) list. Pinning means "never evict or unload
// without an explicit unpin first" - it must be honored on every unload path
// (manual and scheduled), not just the automatic LRU eviction path. Callers
// that want to override this must unpin the model first; there is no
// force-unload bypass.
var ErrModelPinned = errors.New("model is pinned; unpin before unloading")

// ErrUnloadUnsupported is returned by unloadModel for a node whose runtime is
// known and is not Ollama - keep_alive:0 is Ollama-specific, so no unload
// call is made. Distinct from nil (success): a nil return here was
// previously indistinguishable from a genuine successful unload to every
// caller (the manual unload admin endpoint, UnloadModels' direct-path
// fallback, and EvictForHeadroom's free-byte accounting), silently booking
// a phantom eviction that never actually freed any VRAM.
var ErrUnloadUnsupported = errors.New("unload not supported for this runtime")

// modelKey composes the lastUsed map key for a (node, model) pair.
func modelKey(node, model string) string { return node + "\x00" + model }

// RecordModelUse stamps the last-request time for (node, model). Called from the
// proxy on every routed request; this timestamp is what drives LRU eviction
// (the coldest model - oldest or never-seen - is unloaded first under pressure).
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

// recordLastKnownVRAM caches the real, currently-resident VRAM size for
// (node, model), so it remains available as a size estimate after the model
// is later unloaded/evicted (see lastKnownVRAM field doc). Called every poll
// cycle for every currently-loaded model.
func (r *Router) recordLastKnownVRAM(node, model string, bytes int64) {
	if bytes <= 0 {
		return
	}
	r.vramSeenMu.Lock()
	defer r.vramSeenMu.Unlock()
	if r.lastKnownVRAM == nil {
		r.lastKnownVRAM = make(map[string]int64)
	}
	r.lastKnownVRAM[modelKey(node, model)] = bytes
}

// lastKnownVRAMBytes returns the most recent real VRAM size observed for
// (node, model), or 0 if it has never been seen resident.
func (r *Router) lastKnownVRAMBytes(node, model string) int64 {
	r.vramSeenMu.Lock()
	defer r.vramSeenMu.Unlock()
	return r.lastKnownVRAM[modelKey(node, model)]
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

// IsPinned reports whether model is on node's never-evict list. Exported so
// callers that need to enforce the pin policy before choosing how to unload
// (e.g. admin.go deciding between the agent path and the direct Ollama path)
// can check it without going through UnloadModel's own node-specific call.
func (r *Router) IsPinned(node, model string) bool {
	return r.isPinned(node, model)
}

// --- warmup suppression (manual/scheduled unload must stick until re-armed) ---

// suppressedInfo records why and when a (node, model) pair was suppressed -
// the operator-facing detail behind isWarmupSuppressed's plain bool, surfaced
// via SuppressedWarmupInfo/WarmupState so the dashboard can show *why* a
// keep-warm model is sitting cold instead of leaving it unexplained.
type suppressedInfo struct {
	Reason string // "manual_unload" | "scheduled_unload"
	Since  time.Time
}

// suppressWarmup marks (node, model) so pingWarmupModels skips it on its next
// tick, instead of immediately reloading a model an operator just unloaded.
// reason is "manual" or "scheduled" (the same tag recordUnloadSideEffects
// already uses for its log line), stored as the operator-facing enum value.
func (r *Router) suppressWarmup(node, model, reason string) {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	if r.warmupSuppressed == nil {
		r.warmupSuppressed = make(map[string]map[string]suppressedInfo)
	}
	if r.warmupSuppressed[node] == nil {
		r.warmupSuppressed[node] = make(map[string]suppressedInfo)
	}
	enumReason := reason + "_unload"
	r.warmupSuppressed[node][model] = suppressedInfo{Reason: enumReason, Since: time.Now()}
}

// isWarmupSuppressed reports whether (node, model) is currently suppressed.
func (r *Router) isWarmupSuppressed(node, model string) bool {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	_, ok := r.warmupSuppressed[node][model]
	return ok
}

// SuppressedWarmupInfo returns a copy of node's suppressed-model set (model ->
// reason/since), for admin.go to shape into the dashboard-facing WarmupState -
// never the raw map itself, and never a bare bool (see EXECUTION-QUEUE.md's
// "Warmup/unload state is invisible on the dashboard" item for why: a second
// suppression reason was always going to need more than a boolean).
func (r *Router) SuppressedWarmupInfo(node string) map[string]suppressedInfo {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	out := make(map[string]suppressedInfo, len(r.warmupSuppressed[node]))
	for m, info := range r.warmupSuppressed[node] {
		out[m] = info
	}
	return out
}

// clearWarmupSuppress re-arms warmup for the given models on node - called
// when a "warmup" schedule/WarmModels explicitly asks for them to be warm
// again, overriding any earlier unload suppression.
func (r *Router) clearWarmupSuppress(node string, models ...string) {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	for _, m := range models {
		delete(r.warmupSuppressed[node], m)
	}
}

// clearAllWarmupSuppress re-arms warmup for every model on node - called when
// the node's keep-warm configuration itself changes.
func (r *Router) clearAllWarmupSuppress(node string) {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	delete(r.warmupSuppressed, node)
}

// --- keep-warm priority hierarchy (0 = highest priority) ---

// setWarmPriority records the priority order of a node's keep-warm model set,
// ranked (rank) is the model's position in the caller's ordered list - lower
// rank = higher priority. Called once per pingWarmupModels tick so the ranking
// always reflects the current config+toggle order, never a stale copy.
func (r *Router) setWarmPriority(node string, ranked []string) {
	r.warmPriorityMu.Lock()
	defer r.warmPriorityMu.Unlock()
	if r.warmPriority == nil {
		r.warmPriority = make(map[string]map[string]int)
	}
	byModel := make(map[string]int, len(ranked))
	for i, m := range ranked {
		byModel[m] = i
	}
	r.warmPriority[node] = byModel
}

// warmRank returns model's keep-warm priority rank on node (0 = highest), and
// whether model is part of that node's keep-warm set at all.
func (r *Router) warmRank(node, model string) (int, bool) {
	r.warmPriorityMu.RLock()
	defer r.warmPriorityMu.RUnlock()
	rank, ok := r.warmPriority[node][model]
	return rank, ok
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
		return ErrUnloadUnsupported
	}
	body, _ := json.Marshal(map[string]any{"model": model, "keep_alive": 0, "stream": false})
	reqCtx, cancel := context.WithTimeout(ctx, warmupPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, nodeURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.warmupHTTPClient().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("node %s returned %d unloading %q", n.Name, resp.StatusCode, model)
	}
	r.recordUnloadSideEffects(n.Name, model, reason)
	return nil
}

// recordUnloadSideEffects updates every piece of marbor-side bookkeeping that
// must follow a real, successful eviction, regardless of which path
// performed the actual eviction (this router's own direct HTTP call above,
// or a Marbor Agent dispatch - see Router.RecordManualUnload). Skipping this
// for an agent-dispatched unload would silently undo the operator's action:
// without suppressWarmup, pingWarmupModels reloads the model straight back
// into VRAM on its next tick (default 5m later); without DeleteWarmState, a
// marbor restart before the next background flush would restore it from
// SQLite as if it were still resident.
func (r *Router) recordUnloadSideEffects(nodeName, model, reason string) {
	metrics.ModelEvicted(nodeName)
	// A manual or scheduled unload is an operator decision that the model should
	// stay cold; suppress the next warmupTicker ping for it so pingWarmupModels
	// doesn't reload it straight back into VRAM (default 5m later) and silently
	// undo the unload. Re-armed by SetNodeWarmup (config change) or a "warmup"
	// schedule/WarmModels call. LRU-headroom eviction is deliberately excluded -
	// that path evicts a keep-warm model only to make transient room for another
	// request, and must remain eligible to warm back up on the very next tick.
	if reason == "manual" || reason == "scheduled" {
		r.suppressWarmup(nodeName, model, reason)
	}
	// Drop the unloaded model from warm state immediately (Tier 1): a manual,
	// scheduled, or LRU-headroom unload is a residency change that must not wait
	// for the background flush, else a crash could restore an evicted model.
	if st := r.warmStore(); st != nil {
		if err := st.DeleteWarmState(model, nodeName); err != nil {
			log.Printf("warmstate: delete %q on %s after unload: %v", model, nodeName, err)
		}
	}
	log.Printf("unloaded model %q from node %s (%s)", model, nodeName, reason)
}

// RecordManualUnload applies the same post-eviction bookkeeping as a direct
// unload (see recordUnloadSideEffects) for a model actually evicted by a
// Marbor Agent dispatch instead of this router's own HTTP call - the agent
// path has no other way to reach this router-internal state. Always
// "manual" reason: the only caller is the operator-facing unload endpoint.
func (r *Router) RecordManualUnload(nodeName, model string) {
	r.recordUnloadSideEffects(nodeName, model, "manual")
}

// ShouldUseAgentForUnload reports whether nodeName's unload should dispatch
// through its Marbor Agent (enabled + reports capability "models.unload")
// instead of the direct Ollama keep_alive:0 HTTP call - the single decision
// shared by the manual unload endpoint (admin.go handleUnloadModel) and the
// scheduled unload path (UnloadModels below), per P33: a future change to
// the decision has exactly one place to change instead of two. Reliability
// requirement: a node with no agent configured/enabled, or one that hasn't
// reported "models.unload", always gets false here, so the caller's
// pre-existing direct-path fallback runs completely unchanged.
func (r *Router) ShouldUseAgentForUnload(nodeName string) (MarborAgentConfig, bool) {
	cfg, ok := r.MarborAgentSetting(nodeName)
	if !ok || !cfg.Enabled {
		return cfg, false
	}
	return cfg, r.nodeHasAgentCapability(r.FindNode(nodeName), "models.unload")
}

// shouldUseAgentForUnloadNode is ShouldUseAgentForUnload's node-already-resolved
// variant. UnloadModels below has already looked up its target NodeState via
// FindNode before this would otherwise run a second, redundant linear scan
// over r.nodes for the exact same node - this avoids that.
func (r *Router) shouldUseAgentForUnloadNode(n *NodeState, nodeName string) (MarborAgentConfig, bool) {
	cfg, ok := r.MarborAgentSetting(nodeName)
	if !ok || !cfg.Enabled {
		return cfg, false
	}
	return cfg, r.nodeHasAgentCapability(n, "models.unload")
}

// nodeHasAgentCapability reports whether n's live-polled AgentCapabilities
// (agent_poll.go) includes capability. Mirrors admin.go's free function of
// the same purpose - kept as a separate small method since admin and router
// are different packages (same reasoning as buildAgentActionURL/
// buildAgentURL below).
func (r *Router) nodeHasAgentCapability(n *NodeState, capability string) bool {
	if n == nil {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, c := range n.AgentCapabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// unloadModelViaAgent dispatches a model unload to nodeURL's Marbor Agent
// (POST /v1/models/{name...}, capability "models.unload"). Mirrors admin.go's
// unloadModelViaAgent/buildAgentUnloadURL exactly (wire contract, response
// decoding) - duplicated here rather than shared, since router cannot import
// admin (the reverse dependency direction would be a cycle); only the
// agent-vs-direct decision above is shared, per the reliability requirement.
func (r *Router) unloadModelViaAgent(ctx context.Context, nodeURL string, cfg MarborAgentConfig, model string) error {
	scheme := cfg.Scheme
	if scheme == "" {
		scheme = "http"
	}
	actionURL, err := buildAgentUnloadURL(nodeURL, cfg.Port, scheme, model)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	// HTTPClientForNode (tls_dial.go), not a bare &http.Client{} - this call
	// site must go through the same TLS-pinning-aware Transport as the poll
	// path and every admin action-path client (P24, no partially-secured
	// agent where telemetry is HTTPS and unload stays plaintext).
	resp, err := r.HTTPClientForNode(marborAgentUnloadTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("agent unload model failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("agent unload model: could not decode response (status %d)", resp.StatusCode)
	}
	if !out.OK {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("agent returned %d", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

// marborAgentUnloadTimeout bounds how long the scheduled-unload path waits for
// a Marbor agent's POST /v1/models/{name} (unload) response. Matches admin.go's
// nodeUnloadModelTimeout for the manual path.
const marborAgentUnloadTimeout = 30 * time.Second

// buildAgentUnloadURL derives the agent's POST /v1/models/{name} URL from the
// node's own URL (same host, via url.Parse per R5 - never arithmetic port
// derivation), the configured agent port, and the agent's OWN scheme
// (independent of nodeURL's scheme - see store.MarborAgentRecord.Scheme's doc
// comment). model is percent-escaped per "/"-delimited segment so a name
// containing "/" (e.g. "org/repo") lands on the agent side as multiple path
// segments, matching its "{name...}" wildcard route. Mirrors admin.go's
// buildAgentUnloadURL.
func buildAgentUnloadURL(nodeURL string, port int, scheme string, model string) (string, error) {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return "", fmt.Errorf("parse node URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("node URL %q has no host", nodeURL)
	}
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d/v1/models/%s", scheme, u.Hostname(), port, escapeModelPathSegments(model)), nil
}

// escapeModelPathSegments percent-escapes each "/"-delimited segment of a
// model name independently, then rejoins with literal "/". Mirrors admin.go's
// escapeModelPathSegments.
func escapeModelPathSegments(model string) string {
	parts := strings.Split(model, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// FindNode returns the node with the given name, or nil.
func (r *Router) FindNode(name string) *NodeState {
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
// never-evict list - pinning blocks manual unload the same as auto-eviction;
// the operator must unpin first.
func (r *Router) UnloadModel(ctx context.Context, nodeName, model string) (bool, error) {
	n := r.FindNode(nodeName)
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
// goroutine so a slow node can't block the scheduler tick. Unknown nodes are
// skipped. Pinned models are skipped (not unloaded) with a log line, same
// policy as the manual UnloadModel path. A known non-Ollama backend that
// falls through to the direct path below fails with ErrUnloadUnsupported
// (recorded as an UnloadError, same as any other failure) instead of
// silently no-op-ing - only the agent branch makes it work for real.
//
// Dispatches through the node's Marbor Agent (capability "models.unload") when
// ShouldUseAgentForUnload says so - same decision handleUnloadModel makes for
// the manual path (P33) - so a vLLM/TGI/llama.cpp/MLX node's scheduled unload
// works for real instead of silently no-op-ing via the direct
// Ollama-only keep_alive:0 call below. A node with no agent
// configured/enabled/capable falls through to that direct call completely
// unchanged. Waits for every model's unload to finish and returns a non-nil
// error listing any that failed (also recorded into NodeState.UnloadErrors)
// so the caller's schedule status reflects the real outcome.
func (r *Router) UnloadModels(ctx context.Context, nodeName string, models []string) error {
	n := r.FindNode(nodeName)
	if n == nil {
		log.Printf("scheduled unload skipped: node %q not found", nodeName)
		return fmt.Errorf("node %q not found", nodeName)
	}
	agentCfg, useAgent := r.shouldUseAgentForUnloadNode(n, nodeName)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	addFailure := func(m, msg string) {
		mu.Lock()
		failures = append(failures, fmt.Sprintf("%s: %s", m, msg))
		mu.Unlock()
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[router] panic in goroutine: %v", rec)
				}
			}()
			if useAgent {
				n.mu.RLock()
				nodeURL, healthy := n.URL, n.Healthy
				n.mu.RUnlock()
				// Fail-fast on a down node, mirroring handleUnloadModel's
				// agent-branch check: a dead node's URL may still answer with
				// something else entirely on that port, which would otherwise
				// surface as a confusing agent-dispatch error instead of the
				// real reachability problem.
				if !healthy {
					log.Printf("scheduled unload of %q on %s skipped: node is currently unreachable (down)", m, nodeName)
					addFailure(m, "node is currently unreachable (down)")
					return
				}
				actx, cancel := context.WithTimeout(ctx, marborAgentUnloadTimeout)
				defer cancel()
				if err := r.unloadModelViaAgent(actx, nodeURL, agentCfg, m); err != nil {
					log.Printf("scheduled unload of %q on %s failed (agent): %v", m, nodeName, err)
					recordUnloadError(n, m, err)
					addFailure(m, err.Error())
					return
				}
				clearUnloadError(n, m)
				r.recordUnloadSideEffects(nodeName, m, "scheduled")
				return
			}
			if err := r.unloadModel(ctx, n, m, "scheduled"); err != nil {
				log.Printf("scheduled unload of %q on %s failed: %v", m, nodeName, err)
				recordUnloadError(n, m, err)
				addFailure(m, err.Error())
				return
			}
			clearUnloadError(n, m)
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		return fmt.Errorf("%d model(s) failed to unload: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// recordUnloadError and clearUnloadError maintain NodeState.UnloadErrors -
// the scheduled-unload analogue of warmer.go's WarmupErrors bookkeeping, so a
// failed scheduled/agent unload is diagnosable from the dashboard instead of
// only ever appearing in the marbor process's own log output.
func recordUnloadError(n *NodeState, model string, err error) {
	n.Lock()
	if n.UnloadErrors == nil {
		n.UnloadErrors = map[string]string{}
	}
	n.UnloadErrors[model] = err.Error()
	n.Unlock()
}

func clearUnloadError(n *NodeState, model string) {
	n.Lock()
	delete(n.UnloadErrors, model)
	n.Unlock()
}

// EvictForHeadroom unloads the coldest non-pinned models on nodeName until at
// least neededBytes of VRAM is free, or only pinned/higher-priority models
// remain (in which case it logs the unmet pressure - a genuine OOM risk,
// surfaced rather than hidden). Returns the number of models evicted. No-op
// when the node's total VRAM is unknown (nothing to reason about).
//
// forModel is the model this headroom is being made for. If forModel is
// itself part of the node's keep-warm set (see setWarmPriority), any other
// loaded model that outranks it (lower rank = higher priority, e.g. earlier in
// the configured "keep warm" list) is protected from eviction - the same
// higher-priority model always wins a VRAM contest instead of whichever one
// happened to warm last. forModel outside the keep-warm set (manual/predictive/
// scheduled warmup of an arbitrary model) gets no such protection: any
// non-pinned loaded model, keep-warm or not, remains fair game via plain LRU,
// matching prior behavior.
func (r *Router) EvictForHeadroom(ctx context.Context, nodeName, forModel string, neededBytes int64) int {
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
	inFlight := make(map[string]int32, len(target.modelInFlight))
	for name, n := range target.modelInFlight {
		inFlight[name] = n
	}
	target.mu.RUnlock()
	if totalBytes <= 0 {
		return 0
	}
	free := totalBytes - usedBytes

	forModelRank, forModelRanked := r.warmRank(nodeName, forModel)

	evicted := 0
	for free < neededBytes {
		coldIdx := -1
		var coldTime time.Time
		sawInFlightOnly := false
		for i, m := range loaded {
			if r.isPinned(nodeName, m.name) {
				continue
			}
			if forModelRanked {
				if rank, ok := r.warmRank(nodeName, m.name); ok && rank < forModelRank {
					continue // higher-priority keep-warm model: protected
				}
			}
			if inFlight[m.name] > 0 {
				// Actively serving a request right now: its last-used
				// timestamp is stamped once at routing time and never
				// refreshed mid-stream, so it can look coldest and get
				// evicted out from under a live generation. Skip it and
				// fall back to a genuinely idle candidate.
				sawInFlightOnly = true
				continue
			}
			t := r.lastUsedAt(nodeName, m.name)
			if coldIdx == -1 || t.Before(coldTime) {
				coldIdx, coldTime = i, t
			}
		}
		if coldIdx == -1 {
			if sawInFlightOnly {
				log.Printf("headroom: node %s needs %d more free bytes for %q but only pinned/higher-priority/in-flight models remain; cannot make room", nodeName, neededBytes-free, forModel)
			} else {
				log.Printf("headroom: node %s needs %d more free bytes for %q but only pinned/higher-priority models remain; cannot make room", nodeName, neededBytes-free, forModel)
			}
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

// estimateModelSizeBytes estimates the VRAM a not-yet-loaded model needs, in
// priority order: (1) the real VRAM footprint last observed while this model
// was actually resident on this node (lastKnownVRAM) - a model that doesn't
// fully fit in VRAM (partial GPU+CPU split, e.g. a large quantized model on a
// small GPU) can use far less real VRAM than its on-disk weights size, so once
// we've actually seen it loaded, that beats guessing from the file forever
// after; (2) the node's /api/tags on-disk size (a good proxy for GGUF weights
// before the model has ever been observed loaded) - only consulted when
// allowFetch is true, since FetchModelTags can perform a live HTTP call on a
// cache miss and some callers (the streaming request-routing hot path) must
// never block on I/O (R2); (3) non-Ollama runtimes (vllm, tgi, llamacpp, mlx)
// don't expose /api/tags, so FetchModelTags fails or the model is absent from
// the result - fall back to the operator-declared vram_overrides size for that
// node+model (R1: an explicit operator declaration, not a guess). Returns 0
// when the size is unknown by any allowed path so callers can decline to
// evict/warm/reserve blindly.
func (r *Router) estimateModelSizeBytes(nodeURL, model string, allowFetch bool) int64 {
	r.mu.RLock()
	var nodeName string
	var overrideMB int64
	for _, n := range r.nodes {
		if n.URL != nodeURL {
			continue
		}
		nodeName = n.Name
		n.mu.RLock()
		overrideMB = n.VRAMOverrides[model]
		n.mu.RUnlock()
		break
	}
	r.mu.RUnlock()

	if nodeName != "" {
		if known := r.lastKnownVRAMBytes(nodeName, model); known > 0 {
			return known
		}
	}

	if allowFetch {
		if tags, err := r.FetchModelTags(nodeURL); err == nil {
			for _, t := range tags {
				if t.Name == model {
					return t.Size
				}
			}
		}
	}
	if overrideMB > 0 {
		return overrideMB * 1024 * 1024
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
// first and conclude - wrongly - that it has the whole node to itself.
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

// unknownModelReserveBytes is a conservative placeholder reservation used by
// reserveColdStartBytes when a cold-start model's real size cannot yet be
// determined. It is deliberately NOT a size estimate or measurement (R1) - it
// is a scheduling guard only, never surfaced as VRAM telemetry anywhere. Its
// sole purpose is to make PendingPrewarmBytes/free_vram_headroom nonzero for
// the node holding this reservation, so a burst of concurrent cold-start
// requests for a never-seen model see each other's pick instead of all
// reading the same stale "fully free" snapshot and colliding on one node
// (P402). Once the poller confirms the model resident, clearWarmReservation
// drops this placeholder like any other reservation - it never lingers past
// warmReservationTTL either way.
const unknownModelReserveBytes = 2 * 1024 * 1024 * 1024 // 2 GiB

// reserveColdStartBytes records a best-effort VRAM reservation for a request
// that just picked node for a not-yet-warm model. Used on the streaming
// request-routing hot path (Route/selectBestNode/RouteExcluding), so it only
// ever consults already-known, zero-I/O size data (estimateModelSizeBytes with
// allowFetch=false) - never a blocking fetch (R2). When the real size isn't
// already known, this falls back to unknownModelReserveBytes rather than
// reserving nothing: a silent no-op here is what let concurrent cold starts
// for the same never-seen model double-book a node, since neither request's
// pick discounted the other's headroom (P402).
func (r *Router) reserveColdStartBytes(nodeURL, nodeName, model string) {
	est := r.estimateModelSizeBytes(nodeURL, model, false)
	if est <= 0 {
		est = unknownModelReserveBytes
	}
	r.reserveWarmBytes(nodeName, model, est)
}

// chainFor is the shared lookup behind FallbackChainFor and
// LocalDegradationChainFor: both are config-only, immutable-after-
// construction maps, so a plain read needs no locking.
func chainFor(chains map[string][]string, model string) []string {
	return chains[model]
}

// FallbackChainFor returns the operator-declared, ordered list of alternate
// models to try for model, or nil if none is configured. Opt-in only - a
// model absent from routing.fallback_chains has no substitution behavior.
func (r *Router) FallbackChainFor(model string) []string {
	return chainFor(r.fallbackChains, model)
}

// LocalDegradationChainFor returns the operator-declared, ordered list of
// local alternate models to try for model when no node can serve it at all,
// or nil if none is configured. Opt-in only - a model absent from
// routing.local_degradation_chains has no substitution behavior. Single-hop
// only: callers must not recursively resolve a chain for the returned
// alternates.
func (r *Router) LocalDegradationChainFor(model string) []string {
	return chainFor(r.localDegradationChains, model)
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
		size := r.estimateModelSizeBytes(nodeURL, model, true)
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

// hasActiveWarmReservation reports whether (node, model) already has a
// non-expired warm-reservation entry - used by predictive.go's prewarm sites
// to skip triggering a duplicate fire-and-forget load for a model that's
// already mid-warmup, since n.LoadedModels only reflects the last /api/ps
// poll and would otherwise still read "not warm" at the next 5-minute
// predictive cycle. Read-only: unlike reserveWarmBytes, this never writes a
// reservation or drops expired entries itself.
func (r *Router) hasActiveWarmReservation(node, model string) bool {
	r.evictMu.Lock()
	defer r.evictMu.Unlock()
	byModel := r.warmReserved[node]
	if byModel == nil {
		return false
	}
	res, ok := byModel[model]
	if !ok {
		return false
	}
	return time.Since(res.at) <= warmReservationTTL
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
// It runs ONLY on the proactive warm/load path - never on the streaming request
// path - so it never adds latency to a client request.
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
	est := r.estimateModelSizeBytes(nodeURL, model, true)
	if est <= 0 {
		return // unknown size
	}
	if est > totalBytes {
		// This model can never fit on this node no matter what gets evicted -
		// without this check, EvictForHeadroom's loop below would wipe every
		// eligible non-pinned model trying to satisfy a condition that can
		// never be met, and since this re-runs on every subsequent warmup
		// tick (gated only by the cooldown), that full wipe would repeat
		// indefinitely for as long as this oversized model is requested.
		log.Printf("ensureHeadroom: model %s (%d bytes) exceeds node %s's total capacity (%d bytes) - skipping", model, est, nodeName, totalBytes)
		return
	}

	// Reserve this model's estimated footprint now, and pick up whatever other
	// models on this node are still mid-warmup (started, not yet poll-confirmed).
	// Without this, warming two models on the same node races: both read the
	// identical pre-warmup snapshot and each independently - and wrongly  --
	// concludes it has the entire node's free VRAM to itself.
	reservedByOthers := r.reserveWarmBytes(nodeName, model, est)

	// The single-model check above doesn't account for other in-flight
	// reservations - est alone can fit while est+reservedByOthers can't, and
	// EvictForHeadroom has no bound check of its own, so an unsatisfiable
	// neededBytes here would wipe every evictable model on the node on every
	// tick, same failure mode as the est>totalBytes case above.
	if est+reservedByOthers > totalBytes {
		log.Printf("ensureHeadroom: model %s (%d bytes) plus %d bytes reserved by other in-flight warmups exceeds node %s's total capacity (%d bytes) - skipping", model, est, reservedByOthers, nodeName, totalBytes)
		r.clearWarmReservation(nodeName, model)
		return
	}

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
	r.evictMu.Unlock()

	// Only stamp lastEvictAt (starting the cooldown) once EvictForHeadroom
	// actually evicted something (P118) - stamping it unconditionally before
	// the call burns the cooldown even when zero models were evicted (all
	// pinned/higher-priority/in-flight, or an unload error), blocking further
	// auto-eviction attempts on this node for the full window while pressure
	// persists.
	if evicted := r.EvictForHeadroom(ctx, nodeName, model, est+reservedByOthers); evicted > 0 {
		r.evictMu.Lock()
		r.lastEvictAt[nodeName] = time.Now()
		r.evictMu.Unlock()
	}
}
