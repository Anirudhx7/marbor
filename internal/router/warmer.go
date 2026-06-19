package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// warmupPingTimeout is the per-ping HTTP timeout. Loading a model from disk
// for the first time can take minutes on a cold node, so this is deliberately
// generous. Subsequent pings on a warm model complete in <100ms.
const warmupPingTimeout = 5 * time.Minute

// pingWarmupModels sends a zero-token /api/generate with keep_alive to every
// configured (model, node) pair. Each ping runs in its own goroutine so a slow
// node can't block others. Safe to call concurrently.
func (r *Router) pingWarmupModels(ctx context.Context) {
	r.mu.RLock()
	cfg := r.warmupCfg
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	if !cfg.Enabled || len(cfg.Models) == 0 {
		return
	}

	keepAlive := cfg.KeepAlive
	if keepAlive == "" {
		keepAlive = "10m"
	}

	for _, entry := range cfg.Models {
		targets := nodesForEntry(nodes, entry.Nodes)
		for _, n := range targets {
			n := n // capture
			entry := entry
			go func() {
				status := "ok"
				if err := r.pingNode(ctx, n, entry.Model, keepAlive); err != nil {
					status = "error"
				}
				metrics.WarmupPing(entry.Model, n.Name, status)
			}()
		}
	}
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

	resp, err := r.client.Do(req)
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
