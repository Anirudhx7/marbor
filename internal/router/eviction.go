package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
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

// maxTrackedModelsPerNode bounds the per-node last-used and last-known-size
// bookkeeping. Model names in these maps can come straight from client
// requests, so without a cap a client cycling through made-up names would grow
// them without limit. The cap sits far above any real node's model count.
const maxTrackedModelsPerNode = 1024

// canonicalModelName folds the bare-name shorthand onto its ":latest" form so
// "llama3" and "llama3:latest" share one bookkeeping entry, matching
// ModelNamesEquivalent. Digest-pinned and already-tagged names are unchanged.
func canonicalModelName(model string) string {
	if model == "" || strings.Contains(model, "@") || modelNameTagged(model) {
		return model
	}
	return model + ":latest"
}

// modelKey composes the lastUsed map key for a (node, model) pair.
func modelKey(node, model string) string { return node + "\x00" + canonicalModelName(model) }

// RecordModelUse stamps the last-request time for (node, model). Called from the
// proxy on every routed request; this timestamp is what drives LRU eviction
// (the coldest model - oldest or never-seen - is unloaded first under pressure).
func (r *Router) RecordModelUse(node, model string) {
	if node == "" || model == "" {
		return
	}
	// The resident-model set is only needed when this node is at its cap, so
	// it is gathered outside lruMu and only then.
	r.lruMu.Lock()
	full := r.lastUsedFullLocked(node, model)
	r.lruMu.Unlock()
	var loaded map[string]bool
	if full {
		loaded = r.loadedModelSet(node)
	}
	r.lruMu.Lock()
	r.stampLastUsedLocked(node, model, time.Now(), loaded)
	r.lruMu.Unlock()
}

// lastUsedFullLocked reports whether stamping (node, model) would add a new
// entry to a node already at maxTrackedModelsPerNode. The caller holds lruMu.
func (r *Router) lastUsedFullLocked(node, model string) bool {
	if r.lastUsedCount[node] < maxTrackedModelsPerNode {
		return false
	}
	_, exists := r.lastUsed[modelKey(node, model)]
	return !exists
}

// stampLastUsedLocked records at as the last-use time for (node, model) and is
// the single writer of lastUsed, so every path (live requests and the startup
// warm-state restore) obeys the per-node cap. A new entry on a full node first
// trims that node (see trimNodeEntries). loaded names the models currently
// resident on node; nil means unknown. The caller holds lruMu.
func (r *Router) stampLastUsedLocked(node, model string, at time.Time, loaded map[string]bool) {
	if r.lastUsed == nil {
		r.lastUsed = make(map[string]time.Time)
	}
	if r.lastUsedCount == nil {
		r.lastUsedCount = make(map[string]int)
	}
	key := modelKey(node, model)
	if _, exists := r.lastUsed[key]; !exists {
		if r.lastUsedCount[node] >= maxTrackedModelsPerNode {
			r.lastUsedCount[node] = trimNodeEntries(r.lastUsed, node, loaded, func(t time.Time) time.Time { return t })
		}
		r.lastUsedCount[node]++
	}
	r.lastUsed[key] = at
}

// trimBatch is how many entries a full node sheds at once. Trimming in a batch
// amortizes the one scan of the map over many inserts, so a client cycling
// made-up model names cannot force a scan per request.
const trimBatch = maxTrackedModelsPerNode / 8

// trimNodeEntries drops up to trimBatch entries for node from m and returns how
// many remain. Entries for models not currently loaded (per loaded) go first,
// oldest first by stamp, then the oldest overall - so a flood of made-up names
// cannot push a resident model's stamp out. stamp extracts the ordering time
// from a value (the zero time for values with none, which then tie).
func trimNodeEntries[V any](m map[string]V, node string, loaded map[string]bool, stamp func(V) time.Time) int {
	prefix := node + "\x00"
	type entry struct {
		key      string
		at       time.Time
		resident bool
	}
	var entries []entry
	for k, v := range m {
		name, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		entries = append(entries, entry{k, stamp(v), loaded[name]})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].resident != entries[j].resident {
			return !entries[i].resident
		}
		return entries[i].at.Before(entries[j].at)
	})
	drop := trimBatch
	if drop > len(entries) {
		drop = len(entries)
	}
	for _, e := range entries[:drop] {
		delete(m, e.key)
	}
	return len(entries) - drop
}

// loadedModelSet returns the canonical names of the models currently loaded on
// node, for the cap-trim preference. Takes r.mu and the node lock, so callers
// must not hold lruMu or vramSeenMu.
func (r *Router) loadedModelSet(node string) map[string]bool {
	n := r.FindNode(node)
	if n == nil {
		return nil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	set := make(map[string]bool, len(n.LoadedModels))
	for _, m := range n.LoadedModels {
		set[canonicalModelName(m.Name)] = true
	}
	return set
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
	key := modelKey(node, model)
	r.vramSeenMu.Lock()
	_, exists := r.lastKnownVRAM[key]
	full := !exists && r.lastKnownCount[node] >= maxTrackedModelsPerNode
	r.vramSeenMu.Unlock()
	var loaded map[string]bool
	if full {
		loaded = r.loadedModelSet(node)
	}
	r.vramSeenMu.Lock()
	defer r.vramSeenMu.Unlock()
	if r.lastKnownVRAM == nil {
		r.lastKnownVRAM = make(map[string]int64)
	}
	if r.lastKnownCount == nil {
		r.lastKnownCount = make(map[string]int)
	}
	if _, exists := r.lastKnownVRAM[key]; !exists {
		// Same per-node bound as lastUsed. These entries carry no timestamp, so
		// among equals the victim is arbitrary; non-resident models go first. The
		// value is only a size hint and is re-recorded on the next poll while the
		// model is resident.
		if r.lastKnownCount[node] >= maxTrackedModelsPerNode {
			r.lastKnownCount[node] = trimNodeEntries(r.lastKnownVRAM, node, loaded, func(int64) time.Time { return time.Time{} })
		}
		r.lastKnownCount[node]++
	}
	r.lastKnownVRAM[key] = bytes
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
	set := r.pinned[node]
	if set[model] {
		return true
	}
	// Runtimes report the tagged form ("llama3:latest") while operators often
	// pin the bare name, so fall back to name equivalence on an exact miss.
	for p := range set {
		if ModelNamesEquivalent(p, model) {
			return true
		}
	}
	return false
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
// never the raw map itself, and never a bare bool (a second suppression
// reason was always going to need more than a boolean).
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
	ranks := r.warmPriority[node]
	if rank, ok := ranks[model]; ok {
		return rank, true
	}
	// Bare configured names must rank their runtime-reported ":latest" form
	// (and vice versa); take the highest priority (lowest rank) among matches.
	best, found := 0, false
	for name, rank := range ranks {
		if ModelNamesEquivalent(name, model) && (!found || rank < best) {
			best, found = rank, true
		}
	}
	return best, found
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
	// Drain a bounded amount of the reply so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUnloadReplyBytes))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
// scheduled unload path (UnloadModels below), so a future change to
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
	// path and every admin action-path client, so there is no partially-secured
	// agent where telemetry is HTTPS and unload stays plaintext.
	resp, err := r.HTTPClientForNode(marborAgentUnloadTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("agent unload model failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUnloadReplyBytes)).Decode(&out); err != nil {
		return fmt.Errorf("agent unload model: could not decode response (status %d): %w", resp.StatusCode, err)
	}
	// Success needs both: the agent says ok and the HTTP status agrees. An
	// ok:true body on a non-2xx status is a proxy or misbehaving agent, not a
	// confirmed unload.
	if !out.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := truncateForLog(out.Error, maxLogValueBytes)
		if msg == "" {
			msg = fmt.Sprintf("agent returned %d", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

// maxUnloadReplyBytes caps how much of an unload reply is read: the useful
// content is a few bytes of JSON, and nothing here should buffer more.
const maxUnloadReplyBytes = 1 << 20

// marborAgentUnloadTimeout bounds how long the scheduled-unload path waits for
// a Marbor agent's POST /v1/models/{name} (unload) response. Matches admin.go's
// nodeUnloadModelTimeout for the manual path.
const marborAgentUnloadTimeout = 30 * time.Second

// buildAgentUnloadURL derives the agent's POST /v1/models/{name} URL from the
// node's own URL (same host, via url.Parse - never arithmetic port
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
	// JoinHostPort re-brackets an IPv6 literal that Hostname() stripped.
	hostPort := net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	return fmt.Sprintf("%s://%s/v1/models/%s", scheme, hostPort, escapeModelPathSegments(model)), nil
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
// the manual path - so a vLLM/TGI/llama.cpp/MLX node's scheduled unload
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
					pv := truncateForLog(fmt.Sprint(rec), maxLogValueBytes)
					if r.allowHostLog("panic:" + nodeName + "/" + m) {
						log.Printf("scheduled unload of %q on %s panicked: %s\n%s", m, nodeName, pv, debug.Stack())
					}
					perr := fmt.Errorf("panic: %s", pv)
					recordUnloadError(n, m, perr)
					addFailure(m, perr.Error())
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
					recordUnloadError(n, m, errors.New("node is currently unreachable (down)"))
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
	targetURL := target.URL
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
	// FragmentationOverheadMult: allocator/PagedAttention block slack and CUDA
	// graph bookkeeping consume real VRAM beyond the sum of loaded models'
	// reported bytes, so a realistic "free" must discount that.
	free := totalBytes - int64(float64(usedBytes)*FragmentationOverheadMult)

	forModelRank, forModelRanked := r.warmRank(nodeName, forModel)

	// Models a live sticky session is still using on this node get a soft,
	// last-resort protection: they are only chosen as a victim when no
	// unprotected candidate remains. Unlike pinned models this is never
	// absolute - evicting one costs that session its cached context, not
	// correctness.
	sessionModels := r.affinityProtectedModels(targetURL)
	sessionProtected := func(name string) bool {
		for _, m := range sessionModels {
			if ModelNamesEquivalent(m, name) {
				return true
			}
		}
		return false
	}

	evicted := 0
	for free < neededBytes {
		coldIdx := -1
		var coldTime time.Time
		protIdx := -1
		var protTime time.Time
		sawInFlightOnly := false
		for i, m := range loaded {
			if r.isPinned(nodeName, m.name) {
				continue
			}
			if m.size <= 0 {
				// The runtime reports no per-model size (vLLM, TGI, llama.cpp,
				// MLX). Fall back to the last size seen for it; with none,
				// unloading it would free nothing we can account for, so it can
				// never help meet the target.
				known := r.lastKnownVRAMBytes(nodeName, m.name)
				if known <= 0 {
					continue
				}
				loaded[i].size = known
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
			if sessionProtected(m.name) {
				if protIdx == -1 || t.Before(protTime) {
					protIdx, protTime = i, t
				}
				continue
			}
			if coldIdx == -1 || t.Before(coldTime) {
				coldIdx, coldTime = i, t
			}
		}
		if coldIdx == -1 && protIdx != -1 {
			log.Printf("headroom: node %s: only models with a live sticky session remain evictable for %q; evicting %q as a last resort", nodeName, forModel, loaded[protIdx].name)
			coldIdx = protIdx
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
		// Credit what was freed with the same overhead multiplier that was
		// charged for it above, so the running total stays consistent.
		free += int64(float64(victim.size) * FragmentationOverheadMult)
		loaded = append(loaded[:coldIdx], loaded[coldIdx+1:]...)
		evicted++
	}
	return evicted
}

// evictCooldown bounds how often auto-eviction runs per node, so a node under
// sustained pressure can't thrash (rapid load/evict oscillation).
const evictCooldown = 15 * time.Second

// lookupByModelName finds model in m by exact key first, then by name
// equivalence, so "llama3" and "llama3:latest" resolve the same entry.
func lookupByModelName[V any](m map[string]V, model string) (V, bool) {
	if v, ok := m[model]; ok {
		return v, true
	}
	for k, v := range m {
		if ModelNamesEquivalent(k, model) {
			return v, true
		}
	}
	var zero V
	return zero, false
}

// estimateModelSizeBytes estimates the VRAM a not-yet-loaded model needs, in
// priority order: (1) the real VRAM footprint last observed while this model
// was actually resident on this node (lastKnownVRAM) - a model that doesn't
// fully fit in VRAM (partial GPU+CPU split, e.g. a large quantized model on a
// small GPU) can use far less real VRAM than its on-disk weights size, so once
// we've actually seen it loaded, that beats guessing from the file forever
// after, and is returned as-is (it's a real measurement, not a proxy - no
// overhead multiplier applies); (2) the node's /api/tags on-disk size (a good
// proxy for GGUF/safetensors weights before the model has ever been observed
// loaded) - only consulted when allowFetch is true, since FetchModelTags can
// perform a live HTTP call on a cache miss and some callers (the streaming
// request-routing hot path) must never block on I/O. This tier alone
// runs the disk size through EstimateContextAwareBytes so a
// quantization/format overhead multiplier is applied consistently with what
// the Model Advisor already tells the operator (GGUFOverheadMult/
// SafetensorsOverheadMult in kvcache.go) - a Q4_K_M and an F16 build of the
// same model no longer tie purely because they happen to tie on raw disk
// bytes, and requestedCtx (0 when the caller has no context-length signal)
// adds real per-token KV-cache growth on top when available; (3) non-Ollama
// runtimes (vllm, tgi, llamacpp, mlx) don't expose /api/tags, so
// FetchModelTags fails or the model is absent from the result - fall back to
// the operator-declared vram_overrides size for that node+model (an
// explicit operator declaration already accounts for whatever the operator
// knows, so it too is returned as-is with no overhead multiplier). Returns 0
// when the size is unknown by any allowed path so callers can decline to
// evict/warm/reserve blindly.
func (r *Router) estimateModelSizeBytes(nodeURL, model string, allowFetch bool, requestedCtx int64) int64 {
	r.mu.RLock()
	var nodeName, runtime string
	var overrideMB int64
	for _, n := range r.nodes {
		n.mu.RLock()
		if n.URL != nodeURL {
			n.mu.RUnlock()
			continue
		}
		nodeName = n.Name
		overrideMB, _ = lookupByModelName(n.VRAMOverrides, model)
		runtime = n.Runtime
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
				if ModelNamesEquivalent(t.Name, model) {
					arch, hasArch := r.modelArchFactsFor(model)
					var archPtr *ModelArchFacts
					if hasArch {
						archPtr = &arch
					}
					return EstimateContextAwareBytes(t.Size/(1024*1024), requestedCtx, runtime, archPtr)
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
// determined. It is deliberately NOT a size estimate or measurement - it
// is a scheduling guard only, never surfaced as VRAM telemetry anywhere. Its
// sole purpose is to make PendingPrewarmBytes/free_vram_headroom nonzero for
// the node holding this reservation, so a burst of concurrent cold-start
// requests for a never-seen model see each other's pick instead of all
// reading the same stale "fully free" snapshot and colliding on one node.
// Once the poller confirms the model resident, clearWarmReservation
// drops this placeholder like any other reservation - it never lingers past
// warmReservationTTL either way.
const unknownModelReserveBytes = 2 * 1024 * 1024 * 1024 // 2 GiB

// reserveColdStartBytes records a best-effort VRAM reservation for a request
// that just picked node for a not-yet-warm model. Used on the streaming
// request-routing hot path (Route/selectBestNode/RouteExcluding), so it only
// ever consults already-known, zero-I/O size data (estimateModelSizeBytes with
// allowFetch=false) - never a blocking fetch. When the real size isn't
// already known, this falls back to unknownModelReserveBytes rather than
// reserving nothing: a silent no-op here is what let concurrent cold starts
// for the same never-seen model double-book a node, since neither request's
// pick discounted the other's headroom.
func (r *Router) reserveColdStartBytes(nodeURL, nodeName, model string) {
	est := r.estimateModelSizeBytes(nodeURL, model, false, 0)
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
// data (tags-cache size, live VRAM) as predictive prewarm and eviction.
//
// Two distinct "unknown" cases exist here and are handled differently:
//   - No healthy node has a known VRAM *capacity* (VRAMTotalMB, i.e. a
//     marbor-agent report or a manual vram_total_mb override) at all - a
//     remote fleet with no agent has no real capacity signal whatsoever, so
//     there is nothing to fail open ON. Returns false: "not confirmed to
//     fit," never "fits."
//   - Capacity is known on at least one healthy node, but model's *size* is
//     unknown everywhere (never seen, no tags-cache entry) - there is real
//     capacity data, just nothing to compare it against. This still fails
//     open (true), matching the existing safe "never guess a value that
//     wasn't observed" behavior for a genuinely novel model.
//
// requestedCtx, when > 0, is the caller's best available context-length
// signal (typically a live per-request token estimate) for the fit check
// below: the same disk-size model can need materially more VRAM at
// 32K context than at 1K, and a size-only comparison can't see that. <= 0
// means no context-length signal was available, in which case the original
// size-only comparison is used unchanged (never fabricate a context
// length that wasn't observed or configured).
func (r *Router) ModelFitsAnyHealthyNode(model string, requestedCtx int64) bool {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	sawKnownSize := false
	anyCapacityKnown := false
	for _, n := range nodes {
		n.mu.RLock()
		healthy := n.Healthy && !n.Draining
		// FragmentationOverheadMult: see EvictForHeadroom - allocator/CUDA graph
		// slack beyond reported used bytes.
		freeBytes := (n.VRAMTotalMB - int64(float64(n.VRAMUsedMB)*FragmentationOverheadMult)) * 1024 * 1024
		nodeURL := n.URL
		vramKnown := n.VRAMTotalMB > 0
		n.mu.RUnlock()
		if !healthy || !vramKnown {
			continue
		}
		anyCapacityKnown = true
		size := r.estimateModelSizeBytes(nodeURL, model, true, requestedCtx)
		if size <= 0 {
			continue
		}
		sawKnownSize = true
		if freeBytes >= size {
			return true
		}
	}
	if !anyCapacityKnown {
		return false
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
		// A down or not-yet-polled node still counts: its cached tags say the
		// model is on disk there, and rejecting on health would refuse the
		// fallback chain right after startup. Only a failed fetch skips it.
		tags, err := r.FetchModelTags(nodeURL)
		if err != nil {
			continue
		}
		for _, t := range tags {
			if ModelNamesEquivalent(t.Name, model) {
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
	for _, m := range n.LoadedModels {
		usedBytes += m.SizeVRAM
	}
	// Match the way keep-warm names models: a bare name is its ":latest" tag,
	// so "llama3" is already resident when "llama3:latest" is loaded.
	_, resident := findLoadedModel(n.LoadedModels, model)
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
	// When the operator has declared a context window for model, size
	// the disk-size-tier reservation for that context length instead of
	// weights-only - the same disk size needs materially more real VRAM at a
	// large declared context than at a small one. No declared window (the
	// common case for a model nobody has configured a window for) passes 0,
	// exactly the original behavior for the KV-cache-growth term (never
	// guess an undeclared context length) - the quantization/format overhead
	// multiplier still applies either way, entirely inside
	// estimateModelSizeBytes, and only to the disk-size tier (a real observed
	// lastKnownVRAM or an operator override passes through unmodified).
	var requestedCtx int64
	if window, ok := r.contextWindowFor(model); ok && window > 0 {
		requestedCtx = int64(window)
	}
	est := r.estimateModelSizeBytes(nodeURL, model, true, requestedCtx)
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

	// FragmentationOverheadMult: see EvictForHeadroom - allocator/CUDA graph
	// slack beyond reported used bytes.
	adjustedUsed := int64(float64(usedBytes) * FragmentationOverheadMult)
	if totalBytes-adjustedUsed-reservedByOthers >= est {
		return // fits alongside real usage and any other in-flight loads
	}
	// Thrash guard: at most one auto-eviction per node per cooldown window.
	// A second caller that arrives while the first one's eviction is still in
	// flight finds the claim held and returns without evicting; it proceeds on
	// the first caller's reservation view. If the first pass evicts nothing the
	// claim is given back, and later callers evaluate afresh.
	claim, ok := r.claimEvictCooldown(nodeName)
	if !ok {
		return
	}
	evicted := 0
	// Deferred so a panic or a context cancellation inside the eviction also
	// gives the claim back: a pass that evicted nothing (all pinned/higher-
	// priority/in-flight, an unload error, a panic) must not burn the cooldown,
	// which would block further auto-eviction attempts on this node for the full
	// window while pressure persists.
	defer func() {
		if evicted == 0 {
			r.releaseEvictClaim(nodeName, claim)
		}
	}()
	evicted = r.EvictForHeadroom(ctx, nodeName, model, est+reservedByOthers)
}

// evictClaim identifies one claim of a node's eviction cooldown, so releasing
// it can never undo a different caller's newer claim.
type evictClaim struct {
	id      uint64
	prev    time.Time
	hadPrev bool
}

// claimEvictCooldown atomically checks the node's eviction cooldown and, when
// it is clear, claims it in the same critical section. Checking first and
// stamping after the eviction let two concurrent warmups both pass the check
// and each evict, doubling the eviction the guard exists to prevent.
func (r *Router) claimEvictCooldown(nodeName string) (evictClaim, bool) {
	r.evictMu.Lock()
	defer r.evictMu.Unlock()
	if r.lastEvictAt == nil {
		r.lastEvictAt = make(map[string]time.Time)
	}
	if r.evictClaimID == nil {
		r.evictClaimID = make(map[string]uint64)
	}
	prev, hadPrev := r.lastEvictAt[nodeName]
	if hadPrev && time.Since(prev) < evictCooldown {
		return evictClaim{}, false
	}
	r.evictClaimSeq++
	r.evictClaimID[nodeName] = r.evictClaimSeq
	r.lastEvictAt[nodeName] = time.Now()
	return evictClaim{id: r.evictClaimSeq, prev: prev, hadPrev: hadPrev}, true
}

// releaseEvictClaim restores the cooldown state from before claim, but only
// while claim is still the node's latest: a newer claim by another caller is
// left alone.
func (r *Router) releaseEvictClaim(nodeName string, claim evictClaim) {
	r.evictMu.Lock()
	defer r.evictMu.Unlock()
	if r.evictClaimID[nodeName] != claim.id {
		return
	}
	delete(r.evictClaimID, nodeName)
	if claim.hadPrev {
		r.lastEvictAt[nodeName] = claim.prev
	} else {
		delete(r.lastEvictAt, nodeName)
	}
}
