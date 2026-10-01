package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
)

// warmupPingTimeout is the per-ping HTTP timeout. Loading a model from disk
// for the first time can take minutes on a cold node, so this is deliberately
// generous. Subsequent pings on a warm model complete in <100ms.
const warmupPingTimeout = 5 * time.Minute

// warmupHTTPClient returns the *http.Client used exclusively for warmup
// pings, sharing the Router's TLS-pinning transport (HTTPClientForNode) like
// every other node-facing client - previously this was a bare &http.Client{}
// package-level var, falling back to http.DefaultTransport and bypassing
// pinning verification entirely, unlike every other router-to-node path.
// Passing timeout=0 to HTTPClientForNode preserves the original intent: NO
// client-level Timeout, since a cold first-load can take minutes and the
// actual per-ping deadline is enforced via the request context
// (warmupPingTimeout) - the router's other shared client has a 5s Timeout
// for health probing, a hard ceiling that would abort every cold warmup at
// 5s and silently defeat warmup entirely if reused here.
func (r *Router) warmupHTTPClient() *http.Client {
	return r.HTTPClientForNode(0)
}

// tryStartWarmup claims the warmup slot for nodeName, returning false if
// another cycle is already mid-warm for it (see warmupInProgress on Router).
func (r *Router) tryStartWarmup(nodeName string) bool {
	r.warmupInProgressMu.Lock()
	defer r.warmupInProgressMu.Unlock()
	if r.warmupInProgress == nil {
		r.warmupInProgress = map[string]bool{}
	}
	if r.warmupInProgress[nodeName] {
		return false
	}
	r.warmupInProgress[nodeName] = true
	return true
}

// finishWarmup releases the warmup slot claimed by tryStartWarmup.
func (r *Router) finishWarmup(nodeName string) {
	r.warmupInProgressMu.Lock()
	delete(r.warmupInProgress, nodeName)
	r.warmupInProgressMu.Unlock()
}

// lockNodeLoad serializes one model's headroom-then-load sequence
// (ensureHeadroom followed by the warm ping) per node, across every origin:
// the keep-warm pinger, scheduled warmup and predictive prewarm. Without it
// two origins warming different models on the same node both read the same
// pre-load snapshot, both pass the eviction cooldown, and evict from the same
// pool at once - unloading one model twice and more models than a
// back-to-back run would. tryStartWarmup is a different guard: it only stops
// the pinger from overlapping itself. Returns the unlock function.
func (r *Router) lockNodeLoad(nodeName string) func() {
	r.warmupInProgressMu.Lock()
	if r.nodeLoadLocks == nil {
		r.nodeLoadLocks = map[string]*sync.Mutex{}
	}
	mu := r.nodeLoadLocks[nodeName]
	if mu == nil {
		mu = &sync.Mutex{}
		r.nodeLoadLocks[nodeName] = mu
	}
	r.warmupInProgressMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// pingWarmupModels sends a zero-token /api/generate with keep_alive to every
// configured (model, node) pair. Each node warms in its own goroutine so a
// slow node can't block others, but multiple models on the same node are
// pinged one at a time (see the loop below for why). Safe to call
// concurrently.
func (r *Router) pingWarmupModels(ctx context.Context) {
	r.mu.RLock()
	cfg := r.warmupCfg
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	nodeWarmup := make(map[string]NodeWarmup, len(r.nodeWarmup))
	for k, v := range r.nodeWarmup {
		nodeWarmup[k] = v
	}
	r.mu.RUnlock()

	// Replica topology, resolved once per cycle: a replica worker is never a
	// warmup target (keep-warm for a multi-host deployment is managed on its
	// head), and an unresolved node's declaration is not safe to interpret.
	roles, heads := r.SchedulingRolesWithHeads(nodes)

	// Build the effective warm set (nodeName -> ordered models): the union of
	// the config-file warmup (optionally node-scoped) and per-node runtime
	// warmup toggled via the admin API. Order is preserved and deduped
	// (first-seen wins) rather than collapsed into a map, because it doubles
	// as a priority hierarchy: when a node can't fit every keep-warm model at
	// once, earlier-listed models always win the VRAM contest over
	// later-listed ones (see setWarmPriority/EvictForHeadroom) instead of
	// whichever happened to warm last under Go's randomized map iteration.
	//
	// explicit records which sources NAMED a node for a model (a per-node
	// keep-warm entry, or a global entry whose node list lists it), as
	// opposed to a global entry with no node list that expands to every
	// node. It drives the remediation text of the worker-skip warning, since
	// the two sources are cleared through different surfaces.
	byNode := map[string][]string{}
	seen := map[string]map[string]struct{}{}
	explicit := map[string]map[string]warmupSource{}
	add := func(nodeName, model string, src warmupSource) {
		if model == "" {
			return
		}
		// Record provenance before the dedupe return below, so a pair named
		// by both a config entry and a per-node entry keeps both kinds.
		if src != 0 {
			if explicit[nodeName] == nil {
				explicit[nodeName] = map[string]warmupSource{}
			}
			explicit[nodeName][model] |= src
		}
		if seen[nodeName] == nil {
			seen[nodeName] = map[string]struct{}{}
		}
		if _, dup := seen[nodeName][model]; dup {
			return
		}
		seen[nodeName][model] = struct{}{}
		byNode[nodeName] = append(byNode[nodeName], model)
	}
	if cfg.Enabled {
		for _, entry := range cfg.Models {
			var src warmupSource
			if len(entry.Nodes) > 0 {
				src = warmupSourceConfig
			}
			for _, n := range nodesForEntry(nodes, entry.Nodes) {
				add(n.Name, entry.Model, src)
			}
		}
	}
	for name, nw := range nodeWarmup {
		if !nw.Enabled {
			continue
		}
		for _, m := range nw.Models {
			add(name, m, warmupSourceNode)
		}
	}

	// warnings is this cycle's complete WarmupWarnings set, computed
	// synchronously in the loop below (never by the per-node goroutines) and
	// assigned to every node when this function returns, so each warning
	// lives exactly as long as its condition.
	warnings := map[string]map[string]string{}
	warn := func(nodeName, model, msg string) {
		if warnings[nodeName] == nil {
			warnings[nodeName] = map[string]string{}
		}
		warnings[nodeName][model] = msg
	}
	defer func() {
		for _, n := range nodes {
			w := warnings[n.Name]
			n.Lock()
			n.WarmupWarnings = w
			n.Unlock()
		}
	}()

	if len(byNode) == 0 {
		return // nothing to warm - fast no-op (warnings still reset above)
	}

	// The keep_alive we send MUST outlast the warm interval, or the model
	// unloads between pings and users hit a cold start - defeating the point.
	keepAlive := effectiveKeepAlive(cfg.KeepAlive, time.Duration(cfg.IntervalMs)*time.Millisecond)

	nodeByName := make(map[string]*NodeState, len(nodes))
	for _, n := range nodes {
		nodeByName[n.Name] = n
	}
	for nodeName, models := range byNode {
		n := nodeByName[nodeName]
		if n == nil {
			continue
		}
		// Role gate first, for every source: a global entry with no node list
		// must not turn a worker into a warmup target either. Only pairs that
		// explicitly named the worker get a warning - a global entry already
		// expands to the head itself, so nothing is lost there.
		switch roles[nodeName] {
		case RoleWorker:
			for _, model := range models {
				if src := explicit[nodeName][model]; src != 0 {
					warn(nodeName, model, workerSkipWarning(nodeName, heads[nodeName], src))
				}
			}
			continue
		case RoleUnresolved:
			for _, model := range models {
				warn(nodeName, model, "not warmed: node has an unresolved replica declaration - reconcile replica_peers")
			}
			continue
		}
		// Warmup uses Ollama's /api/generate keep_alive; skip non-Ollama backends.
		if !WarmupSupportedForRuntime(n.GetRuntime()) {
			continue
		}
		// A draining node is being emptied - reloading a keep-warm model into
		// it here would silently undo the drain (see DrainNode/UndrainNode).
		// An unhealthy node is skipped too, before ensureHeadroom's eviction
		// bookkeeping runs for a ping that is guaranteed to fail.
		n.mu.RLock()
		draining := n.Draining
		healthy := n.Healthy
		n.mu.RUnlock()
		if draining || !healthy {
			continue
		}
		if !r.isGPUGroupSufficient(n) {
			continue
		}
		// Publish this node's current priority order before warming, so
		// EvictForHeadroom can protect a higher-priority keep-warm model from
		// being evicted to make room for a lower-priority one below.
		r.setWarmPriority(nodeName, models)
		// Residency check (real, not cosmetic): record whether each target model
		// is currently loaded in VRAM on this node, from the latest /api/ps poll.
		// A bare configured name matches its ":latest" resident entry.
		n.mu.RLock()
		loaded := append([]ModelInfo(nil), n.LoadedModels...)
		n.mu.RUnlock()
		// Warm in priority order (highest first) so a higher-priority model is
		// always already resident - and thus protected - by the time a
		// lower-priority sibling's headroom check runs.
		nodeModels := make([]string, 0, len(models))
		for _, model := range models {
			entry, resident := findLoadedModel(loaded, model)
			metrics.WarmupResident(model, n.Name, resident)
			// Digest drift is surfaced, never corrected: the model is still
			// kept warm, since skipping the ping would let it expire from VRAM
			// as a silent side effect.
			if resident && r.digestMismatch(entry.Name, entry.Digest) {
				warn(nodeName, model, "digest drift: resident copy differs from the fleet's reference digest for this model; still being kept warm")
			}
			nodeModels = append(nodeModels, model)
		}
		if !r.tryStartWarmup(nodeName) {
			// A prior cycle (ticker, startup ping, or a manual TriggerWarmup)
			// is still mid-warm for this node - skip rather than dispatch a
			// second overlapping request for the same (node, model) pairs.
			continue
		}
		go func() {
			defer r.finishWarmup(nodeName)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[router] panic in goroutine: %v", r)
				}
			}()
			// Different nodes still warm fully in parallel (this goroutine is
			// per-node, so a slow node can't block others). But models on the
			// SAME node are warmed one at a time, not fired concurrently:
			// n.LoadedModels only reflects the last /api/ps poll, so firing
			// several cold /api/generate loads at once against one node makes
			// ensureHeadroom's capacity check race (each sees the identical
			// pre-warmup snapshot) and hands the real runtime two competing
			// concurrent loads it must arbitrate itself. Loading one model
			// fully before starting the next keeps headroom accounting honest
			// and avoids the runtime evicting one warmed model to satisfy the
			// other.
			for _, model := range nodeModels {
				// A manual/scheduled unload suppressed this model - skip it so
				// this tick doesn't silently reload what the operator just took
				// cold (see suppressWarmup in eviction.go).
				if r.isWarmupSuppressed(n.Name, model) {
					continue
				}
				// Make VRAM room (evict coldest non-pinned) before loading, so
				// warming several models on a tight node can't OOM.
				unlock := r.lockNodeLoad(n.Name)
				r.ensureHeadroom(ctx, n, model)
				err := r.pingNode(ctx, n, model, keepAlive)
				unlock()
				status := "ok"
				if err != nil {
					status = "error"
					// Warmup failed - release the reservation now instead of
					// letting it block other models' headroom checks for the
					// remainder of warmReservationTTL.
					r.clearWarmReservation(n.Name, model)
					log.Printf("[warmup] node %s model %s: %v", n.Name, model, err)
					n.Lock()
					if n.WarmupErrors == nil {
						n.WarmupErrors = map[string]string{}
					}
					n.WarmupErrors[model] = err.Error()
					n.Unlock()
				} else {
					n.Lock()
					delete(n.WarmupErrors, model)
					n.Unlock()
				}
				metrics.WarmupPing(model, n.Name, status)
			}
		}()
	}
}

// warmupSource is a bit set of the configuration sources that explicitly
// named a node for a keep-warm model.
type warmupSource uint8

const (
	// warmupSourceNode: the node's own keep-warm setting
	// (PUT /admin/nodes/{name}/warmup).
	warmupSourceNode warmupSource = 1 << iota
	// warmupSourceConfig: a global warmup entry whose node list names the
	// node (the warmup_models setting).
	warmupSourceConfig
)

// workerSkipWarning builds the notice shown for a keep-warm model explicitly
// configured on a replica worker, with remediation matching each source that
// named it - the per-node setting and the global setting are cleared through
// different surfaces, and clearing one never touches the other.
func workerSkipWarning(node, head string, src warmupSource) string {
	msg := fmt.Sprintf("not warmed: this node is a replica worker; keep-warm is managed on its head %s.", head)
	if src&warmupSourceNode != 0 {
		msg += fmt.Sprintf(" Clear this node's stored keep-warm config (Warmup page 'Clear stale config', or: marbor nodes warmup set %s --enabled=false --models \"\") and add the model on %s instead.", node, head)
	}
	if src&warmupSourceConfig != 0 {
		msg += fmt.Sprintf(" Remove %s from the node list of the global warmup entry for this model (warmup models in the global settings, PUT /admin/settings) and list %s instead.", node, head)
	}
	return msg
}

// effectiveKeepAlive returns a keep_alive value guaranteed to outlast the warm
// interval so a model can never unload between pings. If the configured value is
// empty or parses to less than the interval, it is bumped to 2x the interval.
func effectiveKeepAlive(configured string, interval time.Duration) string {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if configured != "" {
		if d, err := time.ParseDuration(configured); err == nil && d >= interval {
			return configured
		}
	}
	return (2 * interval).String()
}

// pingNode sends a single keep_alive ping for model to the given node.
func (r *Router) pingNode(ctx context.Context, n *NodeState, model, keepAlive string) error {
	n.mu.RLock()
	healthy := n.Healthy
	nodeURL := n.URL
	n.mu.RUnlock()
	if !healthy {
		return fmt.Errorf("node %s unhealthy", n.Name)
	}

	err := r.pingEndpoint(ctx, nodeURL, "/api/generate", map[string]any{
		"model":      model,
		"keep_alive": keepAlive,
		"stream":     false,
	})
	if err == nil {
		return nil
	}
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusBadRequest {
		return err
	}
	// Embedding-only models (e.g. hf.co/mixedbread-ai/mxbai-embed-large-v1)
	// reject /api/generate outright with a 400 "does not support generate" -
	// Ollama only special-cases that capability check away for the
	// keep_alive:0 unload path, not a warming ping. Retry via /api/embed,
	// which loads the model into VRAM the same way. Any other status (auth,
	// 5xx, network) is returned as-is - a 400 is the specific, well-known
	// signature of "wrong endpoint for this model type", not a generic
	// failure worth masking with a second request.
	if embedErr := r.pingEndpoint(ctx, nodeURL, "/api/embed", map[string]any{
		"model":      model,
		"input":      "",
		"keep_alive": keepAlive,
	}); embedErr != nil {
		return fmt.Errorf("node %s: generate: %v; embed: %v", n.Name, err, embedErr)
	}
	return nil
}

// statusError wraps a non-2xx HTTP response so callers can branch on the
// status code without parsing the error string.
type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("returned %d", e.status) }

// pingEndpoint POSTs payload to path on nodeURL and treats any 4xx/5xx
// response as a *statusError.
func (r *Router) pingEndpoint(ctx context.Context, nodeURL, path string, payload map[string]any) error {
	body, _ := json.Marshal(payload)

	reqCtx, cancel := context.WithTimeout(ctx, warmupPingTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, nodeURL+path, bytes.NewReader(body))
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
		return &statusError{status: resp.StatusCode}
	}
	return nil
}

// nodesForEntry returns the nodes that should receive a warmup ping for entry.
// Empty allowList = all nodes; non-empty = only nodes whose Name is in the list.
func nodesForEntry(nodes []*NodeState, allowList []string) []*NodeState {
	if len(allowList) == 0 {
		return nodes
	}
	set := make(map[string]struct{}, len(allowList))
	for _, name := range allowList {
		set[name] = struct{}{}
	}
	var out []*NodeState
	for _, n := range nodes {
		if _, ok := set[n.Name]; ok {
			out = append(out, n)
		}
	}
	return out
}
