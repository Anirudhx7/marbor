package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// warmupPingTimeout is the per-ping HTTP timeout. Loading a model from disk
// for the first time can take minutes on a cold node, so this is deliberately
// generous. Subsequent pings on a warm model complete in <100ms.
const warmupPingTimeout = 5 * time.Minute

// warmupHTTPClient is used exclusively for warmup pings. It intentionally has
// NO client-level Timeout: a cold first-load can take minutes, and the actual
// per-ping deadline is enforced via the request context (warmupPingTimeout).
// The router's shared r.client has a 5s Timeout for health probing, which is a
// hard ceiling that overrides any longer request context - using it here would
// abort every cold warmup at 5s and silently defeat warmup entirely.
var warmupHTTPClient = &http.Client{}

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

	// Build the effective warm set (nodeName -> ordered models): the union of
	// the config-file warmup (optionally node-scoped) and per-node runtime
	// warmup toggled via the admin API. Order is preserved and deduped
	// (first-seen wins) rather than collapsed into a map, because it doubles
	// as a priority hierarchy: when a node can't fit every keep-warm model at
	// once, earlier-listed models always win the VRAM contest over
	// later-listed ones (see setWarmPriority/EvictForHeadroom) instead of
	// whichever happened to warm last under Go's randomized map iteration.
	byNode := map[string][]string{}
	seen := map[string]map[string]struct{}{}
	add := func(nodeName, model string) {
		if model == "" {
			return
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
			for _, n := range nodesForEntry(nodes, entry.Nodes) {
				add(n.Name, entry.Model)
			}
		}
	}
	for name, nw := range nodeWarmup {
		if !nw.Enabled {
			continue
		}
		for _, m := range nw.Models {
			add(name, m)
		}
	}
	if len(byNode) == 0 {
		return // nothing to warm - fast no-op
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
		// Warmup uses Ollama's /api/generate keep_alive; skip non-Ollama backends.
		if rt := n.GetRuntime(); rt != "ollama" && rt != "" {
			continue
		}
		// A draining node is being emptied - reloading a keep-warm model into
		// it here would silently undo the drain (see DrainNode/UndrainNode).
		n.mu.RLock()
		draining := n.Draining
		n.mu.RUnlock()
		if draining {
			continue
		}
		// Publish this node's current priority order before warming, so
		// EvictForHeadroom can protect a higher-priority keep-warm model from
		// being evicted to make room for a lower-priority one below.
		r.setWarmPriority(nodeName, models)
		// Residency check (real, not cosmetic): record whether each target model
		// is currently loaded in VRAM on this node, from the latest /api/ps poll.
		n.mu.RLock()
		loaded := make(map[string]struct{}, len(n.LoadedModels))
		for _, m := range n.LoadedModels {
			loaded[m.Name] = struct{}{}
		}
		n.mu.RUnlock()
		// Warm in priority order (highest first) so a higher-priority model is
		// always already resident - and thus protected - by the time a
		// lower-priority sibling's headroom check runs.
		nodeModels := make([]string, 0, len(models))
		for _, model := range models {
			_, resident := loaded[model]
			metrics.WarmupResident(model, n.Name, resident)
			nodeModels = append(nodeModels, model)
		}
		go func() {
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
				r.ensureHeadroom(ctx, n, model)
				status := "ok"
				if err := r.pingNode(ctx, n, model, keepAlive); err != nil {
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

	resp, err := warmupHTTPClient.Do(req)
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
