package router

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// modelKey composes the lastUsed map key for a (node, model) pair.
func modelKey(node, model string) string { return node + "\x00" + model }

// RecordModelUse stamps the last-request time for (node, model). Called from the
// proxy on every routed request; this timestamp is what drives LRU eviction
// (the coldest model — oldest or never-seen — is unloaded first under pressure).
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
// backends support this; others are a no-op.
func (r *Router) unloadModel(ctx context.Context, n *NodeState, model string) error {
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
	metrics.ModelEvicted(n.Name)
	log.Printf("evicted model %q from node %s (LRU headroom)", model, n.Name)
	return nil
}

// EvictForHeadroom unloads the coldest non-pinned models on nodeName until at
// least neededBytes of VRAM is free, or only pinned models remain (in which case
// it logs the unmet pressure — a genuine OOM risk, surfaced rather than hidden).
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
		if err := r.unloadModel(ctx, target, victim.name); err != nil {
			log.Printf("headroom: failed to evict %q from %s: %v", victim.name, nodeName, err)
			break
		}
		free += victim.size
		loaded = append(loaded[:coldIdx], loaded[coldIdx+1:]...)
		evicted++
	}
	return evicted
}
