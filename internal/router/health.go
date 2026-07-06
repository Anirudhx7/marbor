package router

// health.go - Backend health monitoring and telemetry collection.

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
	runtimepkg "github.com/ollama-mesh/ollama-mesh/internal/runtime"
)

// pollNvidiaAll refreshes nvidia-smi stats for all local nodes and stores
// results in nvidiaCache. Called on a separate ticker (default 30s) so that
// nvidia-smi is never forked on every /api/ps poll cycle.
func (r *Router) pollNvidiaAll() {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	hasLocal := false
	for _, n := range nodes {
		if isLocalNode(n.URL) {
			hasLocal = true
			break
		}
	}
	if !hasLocal {
		return
	}

	gpus, ok := queryAllGPUs()
	r.nvidiaMu.Lock()
	if ok {
		r.nvidiaCache = gpus
	} else {
		r.nvidiaCache = make(map[int]GPUStats)
	}
	r.nvidiaMu.Unlock()
}

// discoverAndAddDockerNodes queries the Docker socket and adds any new
// Ollama containers as nodes. Already-known URLs are skipped.
func (r *Router) discoverAndAddDockerNodes() {
	r.mu.RLock()
	enabled := r.dockerCfg.Enabled
	socket := r.dockerCfg.Socket
	r.mu.RUnlock()
	if !enabled {
		return
	}

	found, err := discoverDockerNodes(socket)
	if err != nil {
		// Docker not available or socket missing — log silently, don't crash.
		return
	}
	for _, n := range found {
		r.mu.Lock()
		_, exists := r.discoveredURLs[n.URL]
		if !exists {
			r.discoveredURLs[n.URL] = struct{}{}
		}
		r.mu.Unlock()
		if !exists {
			r.AddNode(n)
		}
	}
}

func (r *Router) pollAll() {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(node *NodeState) {
			defer wg.Done()
			r.pollNode(node)
		}(n)
	}
	wg.Wait()
}

func (r *Router) pollNode(n *NodeState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// If auto-detect is pending, probe the node now to determine its runtime.
	// Use n.mu to guard the read, then r.client (Router-level, read-only after New).
	n.mu.RLock()
	needsDetect := n.autoDetect && n.Runtime == "auto"
	n.mu.RUnlock()
	if needsDetect {
		detected, reached := runtimepkg.DetectRuntime(ctx, n.URL, r.client)
		if !reached {
			// Node was never actually contacted (transport-level failure) -
			// leave autoDetect pending so the next poll interval retries,
			// instead of permanently committing the "ollama" fallback.
			r.markFailure(n)
			return
		}
		n.mu.Lock()
		n.Runtime = detected
		n.probe = runtimepkg.NewProbe(detected, r.client)
		n.autoDetect = false
		n.mu.Unlock()
		log.Printf("auto-detect: node %s resolved to runtime %q", n.Name, detected)
	}

	n.mu.RLock()
	probe := n.probe
	n.mu.RUnlock()

	result, err := probe.Probe(ctx, n.URL)
	if err != nil {
		r.markFailure(n)
		return
	}
	// Convert runtime.LoadedModel slice to []ModelInfo (router's internal type).
	models := make([]ModelInfo, len(result.LoadedModels))
	for i, m := range result.LoadedModels {
		models[i] = ModelInfo{Name: m.Name, SizeVRAM: m.SizeVRAMBytes}
	}
	psUsedMB := result.VRAMUsedMB

	// nvidia-smi only describes GPUs on the mesh host itself. Read from the
	// nvidiaCache populated by pollNvidiaAll() on its own slower ticker (default
	// 30s) rather than calling queryGPU() here, which would fork nvidia-smi on
	// every /api/ps poll cycle and cause measurable CPU overhead.
	var gpu GPUStats
	hasGPU := false
	if isLocalNode(n.URL) {
		r.nvidiaMu.RLock()
		gpu, hasGPU = r.nvidiaCache[n.NvidiaIndex]
		r.nvidiaMu.RUnlock()
	}

	n.mu.Lock()
	prevModels := n.LoadedModels
	n.LoadedModels = models
	n.Healthy = true
	n.Failures = 0
	n.LastPollAt = time.Now()
	n.Uptime = formatUptime(time.Since(n.FirstSeenAt))
	switch {
	case hasGPU:
		// Richest source: live local nvidia-smi (total, used, temp, power).
		n.VRAMTotalMB = gpu.VRAMTotalMB
		n.VRAMUsedMB = gpu.VRAMUsedMB
		n.PowerDrawW = gpu.PowerDrawW
		temp := gpu.TempCelsius
		n.Temperature = &temp
		n.VRAMSource = "nvidia"
	default:
		// Remote node (or no local GPU): used-VRAM is real, summed from the node's
		// own /api/ps. Total is operator-declared if present. Temp/power unknown.
		n.VRAMUsedMB = psUsedMB
		n.PowerDrawW = 0
		n.Temperature = nil
		if n.VRAMTotalMBConfig > 0 {
			n.VRAMTotalMB = n.VRAMTotalMBConfig
			n.VRAMSource = "declared"
		} else {
			n.VRAMTotalMB = 0
			if psUsedMB > 0 {
				n.VRAMSource = "api"
			} else {
				n.VRAMSource = "none"
			}
		}
	}
	n.HealthHistory = append(n.HealthHistory, 100.0)
	if len(n.HealthHistory) > 60 {
		n.HealthHistory = n.HealthHistory[len(n.HealthHistory)-60:]
	}
	nodeName := n.Name
	nodeURL := n.URL
	n.mu.Unlock()
	metrics.NodeHealthy(n.Name, 1)

	// Persist any residency change (models loaded/unloaded since the last poll)
	// immediately — Tier 1 lifecycle events must not wait for the background flush.
	r.persistResidencyDiff(nodeName, prevModels, models)

	// Reconcile SQLite against live /api/ps truth: delete any warm_state row for
	// this node whose model is not currently resident. This runs on every
	// successful poll so stale rows from a prior process run are pruned
	// deterministically, regardless of restore/poll ordering.
	r.reconcileNodeResidency(nodeName, models)

	// Fire webhook on recovery (unhealthy -> healthy transition). Guarded by
	// a pool-membership check so a poll racing RemoveNode cannot resurrect a
	// prevHealthy entry for a node that was just removed.
	r.mu.Lock()
	if !r.nodeExistsLocked(nodeName) {
		r.mu.Unlock()
		return
	}
	prev, seen := r.prevHealthy[nodeName]
	if seen && !prev {
		r.prevHealthy[nodeName] = true
		r.mu.Unlock()
		r.fireWebhook("node_up", nodeName, nodeURL)
	} else {
		if !seen {
			r.prevHealthy[nodeName] = true
		}
		r.mu.Unlock()
	}
}

// isLocalNode reports whether a node URL points at the mesh host itself, so that
// local nvidia-smi telemetry may be attributed to it. Remote nodes must not be
// given the mesh host's GPU stats.
func isLocalNode(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	return fmt.Sprintf("%dh", hours)
}

func (r *Router) markFailure(n *NodeState) {
	n.mu.Lock()
	n.Failures++
	becameUnhealthy := false
	if n.Failures >= 3 && n.Healthy {
		n.Healthy = false
		becameUnhealthy = true
		metrics.NodeHealthy(n.Name, 0)
	} else if n.Failures >= 3 {
		metrics.NodeHealthy(n.Name, 0)
	}
	healthScore := 0.0
	n.HealthHistory = append(n.HealthHistory, healthScore)
	if len(n.HealthHistory) > 60 {
		n.HealthHistory = n.HealthHistory[len(n.HealthHistory)-60:]
	}
	nodeName := n.Name
	nodeURL := n.URL
	n.mu.Unlock()

	if becameUnhealthy {
		// Capture the node's last-known warm set immediately (Tier 1): once it is
		// unhealthy, polling can no longer refresh its residency.
		r.snapshotNode(n)
		// Fire webhook on node_down transition. Guarded by a pool-membership
		// check so a poll racing RemoveNode cannot resurrect a prevHealthy
		// entry for a node that was just removed.
		r.mu.Lock()
		exists := r.nodeExistsLocked(nodeName)
		if exists {
			r.prevHealthy[nodeName] = false
		}
		r.mu.Unlock()
		if exists {
			r.fireWebhook("node_down", nodeName, nodeURL)
		}
	}
}
