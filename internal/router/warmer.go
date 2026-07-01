package router

import (
	"bytes"
	"context"
	"encoding/json"
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
// configured (model, node) pair. Each ping runs in its own goroutine so a slow
// node can't block others. Safe to call concurrently.
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

	// Build the effective warm set (nodeName -> set of models): the union of the
	// config-file warmup (optionally node-scoped) and per-node runtime warmup
	// toggled via the admin API.
	byNode := map[string]map[string]struct{}{}
	add := func(nodeName, model string) {
		if model == "" {
			return
		}
		if byNode[nodeName] == nil {
			byNode[nodeName] = map[string]struct{}{}
		}
		byNode[nodeName][model] = struct{}{}
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
		if n.Runtime != "ollama" && n.Runtime != "" {
			continue
		}
		// Residency check (real, not cosmetic): record whether each target model
		// is currently loaded in VRAM on this node, from the latest /api/ps poll.
		n.mu.RLock()
		loaded := make(map[string]struct{}, len(n.LoadedModels))
		for _, m := range n.LoadedModels {
			loaded[m.Name] = struct{}{}
		}
		n.mu.RUnlock()
		for model := range models {
			_, resident := loaded[model]
			metrics.WarmupResident(model, n.Name, resident)
			n := n
			model := model
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[router] panic in goroutine: %v", r)
					}
				}()
				// Make VRAM room (evict coldest non-pinned) before loading, so
				// warming several models on a tight node can't OOM.
				r.ensureHeadroom(ctx, n, model)
				status := "ok"
				if err := r.pingNode(ctx, n, model, keepAlive); err != nil {
					status = "error"
				}
				metrics.WarmupPing(model, n.Name, status)
			}()
		}
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

	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"keep_alive": keepAlive,
		"stream":     false,
	})

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
		return fmt.Errorf("node %s returned %d", n.Name, resp.StatusCode)
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
