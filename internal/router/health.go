package router

// health.go - Backend health monitoring and telemetry collection.

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
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
		// Docker not available or socket missing - log silently, don't crash.
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
	// Hard reliability boundary: pollNode runs per-node, on its own goroutine,
	// including synchronously off AddNode at boot for every persisted node.
	// One node's bad/unexpected state (a stale DB record, a malformed probe,
	// anything) must never be able to panic and take down the entire
	// single-process marbor (architecture law) - that would mean a single bad
	// row in marbor.db locks an operator out of their whole fleet, which is
	// exactly the failure this recovers from. A panic here degrades only
	// this one node (marked unhealthy for this cycle) instead of crashing
	// the process; the poll loop naturally retries it next interval.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("router: recovered panic in pollNode for node %s: %v", n.Name, rec)
			r.markFailure(n)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// n.URL is read repeatedly through this function. It used to be safe to
	// read unguarded because it was set once at construction and never
	// mutated again - but Router.AddNode's upsert-by-name path (see its doc
	// comment) can now rewrite an existing, live NodeState's URL from a
	// concurrent goroutine (a fresh POST /admin/nodes for the same name
	// while a poll cycle for that same node is in flight). Snapshot it once
	// under n.mu, same pattern ProbeNodeOnDemand already uses, so the rest
	// of this poll operates on one consistent URL instead of racing.
	n.mu.RLock()
	nodeURL := n.URL
	needsDetect := n.autoDetect && n.Runtime == "auto"
	n.mu.RUnlock()
	if needsDetect {
		detected, reached := runtimepkg.DetectRuntime(ctx, nodeURL, r.client)
		if !reached {
			// Node was never actually contacted (transport-level failure) -
			// leave autoDetect pending so the next poll interval retries,
			// instead of permanently committing the "ollama" fallback. Agent
			// telemetry is polled independently now (see pollAgentHosts,
			// once per host per interval, not nested in this per-node
			// runtime-health poll) - unaffected by this node's /api/ps
			// reachability, so no call needed here.
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
	if probe == nil {
		// Defense in depth: every path that clears autoDetect must also set
		// probe (New/AddNode/PatchNode, and the auto-detect branch above).
		// A nil probe here would otherwise panic and take down the whole
		// marbor process (single-process architecture - R1/architecture law).
		// If this is ever hit, something upstream regressed that invariant;
		// treat it exactly like an unreachable node instead of crashing.
		r.markFailure(n)
		return
	}

	result, err := probe.Probe(ctx, nodeURL)
	if err != nil {
		r.markFailure(n)
		return
	}
	// Convert runtime.LoadedModel slice to []ModelInfo (router's internal type).
	models := make([]ModelInfo, len(result.LoadedModels))
	for i, m := range result.LoadedModels {
		models[i] = ModelInfo{Name: m.Name, SizeVRAM: m.SizeVRAMBytes, Digest: m.Digest}
		r.recordModelDigest(m.Name, m.Digest)
	}
	psUsedMB := result.VRAMUsedMB

	// nvidia-smi only describes GPUs on the marbor host itself. Read from the
	// nvidiaCache populated by pollNvidiaAll() on its own slower ticker (default
	// 30s) rather than calling queryGPU() here, which would fork nvidia-smi on
	// every /api/ps poll cycle and cause measurable CPU overhead.
	var gpu GPUStats
	hasGPU := false
	if isLocalNode(nodeURL) {
		r.nvidiaMu.RLock()
		gpu, hasGPU = r.nvidiaCache[n.NvidiaIndex]
		r.nvidiaMu.RUnlock()
	}

	n.mu.Lock()
	prevModels := n.LoadedModels
	n.LoadedModels = models
	n.Failures = 0
	if !n.Healthy {
		// Flapping hysteresis: require N consecutive successes before putting
		// a previously-unhealthy node back into rotation, so one lucky poll
		// after a real outage doesn't immediately re-route traffic to it.
		n.ConsecutiveSuccesses++
		if n.ConsecutiveSuccesses >= r.healthSuccessThreshold {
			n.Healthy = true
		}
	}
	n.LastPollAt = time.Now()
	n.Uptime = formatUptime(time.Since(n.FirstSeenAt))
	shouldThermalDrain := false
	switch {
	case hasGPU:
		// Richest source: live local nvidia-smi (total, used, temp, power).
		n.VRAMTotalMB = gpu.VRAMTotalMB
		n.VRAMUsedMB = gpu.VRAMUsedMB
		n.PowerDrawW = gpu.PowerDrawW
		temp := gpu.TempCelsius
		n.Temperature = &temp
		n.VRAMSource = "nvidia"

		// Sustained Degradation Auto-Drain: count consecutive polls at/above
		// the configured threshold and drain via the existing DrainNode path
		// once met. One-directional - recovery requires an admin to undrain
		// manually, since a temperature dip doesn't confirm the underlying
		// hardware issue is resolved. Only flips the existing Draining bool
		// (already a Hard-Constraint exclusion) - no routing/scoring change.
		if r.thermalWatchdog.Enabled && r.thermalWatchdog.MaxTempCelsius > 0 {
			if temp >= r.thermalWatchdog.MaxTempCelsius {
				n.ThermalBreaches++
				if n.ThermalBreaches >= r.thermalWatchdog.ConsecutiveBreaches && !n.Draining {
					shouldThermalDrain = true
				}
			} else {
				n.ThermalBreaches = 0
			}
		}
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
	nowHealthy := n.Healthy
	n.mu.Unlock()
	for _, m := range models {
		r.recordLastKnownVRAM(nodeName, m.Name, m.SizeVRAM)
		// Residency is now confirmed by real poll data - drop any hot-path or
		// proactive reservation for this (node, model) so it can't keep
		// double-counting against the now-real VRAMUsedMB for up to the full
		// warmReservationTTL (P51).
		r.clearWarmReservation(nodeName, m.Name)
	}
	if nowHealthy {
		metrics.NodeHealthy(n.Name, 1)
	} else {
		metrics.NodeHealthy(n.Name, 0)
	}

	if shouldThermalDrain {
		r.DrainNode(nodeName, "thermal")
		log.Printf("thermal watchdog: node %s auto-drained after %d consecutive polls at/above %.1f°C",
			nodeName, r.thermalWatchdog.ConsecutiveBreaches, r.thermalWatchdog.MaxTempCelsius)
	}

	// Persist any residency change (models loaded/unloaded since the last poll)
	// immediately - Tier 1 lifecycle events must not wait for the background flush.
	r.persistResidencyDiff(nodeName, prevModels, models)

	// Reconcile SQLite against live /api/ps truth: delete any warm_state row for
	// this node whose model is not currently resident. This runs on every
	// successful poll so stale rows from a prior process run are pruned
	// deterministically, regardless of restore/poll ordering.
	r.reconcileNodeResidency(nodeName, models)

	// Fire webhook on recovery (unhealthy -> healthy transition). Gated on
	// nowHealthy so a successful poll during the hysteresis window (node not
	// yet past HealthSuccessThreshold) doesn't fire node_up prematurely.
	// Guarded by a pool-membership check so a poll racing RemoveNode cannot
	// resurrect a prevHealthy entry for a node that was just removed.
	if !nowHealthy {
		return
	}
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

// ProbeNodeOnDemand runs a synchronous, read-only liveness probe for the
// named node right now, using the same RuntimeProbe pollNode uses on its
// periodic cycle. Unlike pollNode, it never mutates NodeState (Healthy,
// LastPollAt, Failures, ConsecutiveSuccesses are left exactly as the
// periodic poller last set them, and no webhook/SQLite write happens
// here): an admin-triggered one-off "check now" click must not reset the
// poller's own failure-count/hysteresis state outside its normal cadence.
// The periodic poller reconciles real state on its own schedule regardless
// (State Hierarchy - this is a diagnostic read, not a new authoritative
// state write). found is false only if name doesn't match any current
// node; a found node with a not-yet-detected runtime (autoDetect pending,
// probe nil) reports a real error rather than a fabricated result (R1).
func (r *Router) ProbeNodeOnDemand(ctx context.Context, name string) (ok bool, errMsg string, latencyMs int64, found bool) {
	r.mu.RLock()
	var n *NodeState
	for _, node := range r.nodes {
		if node.Name == name {
			n = node
			break
		}
	}
	r.mu.RUnlock()
	if n == nil {
		return false, "", 0, false
	}

	n.RLock()
	nodeURL := n.URL
	probe := n.probe
	n.RUnlock()
	if probe == nil {
		return false, "node runtime not yet detected - still auto-detecting", 0, true
	}

	start := time.Now()
	_, err := probe.Probe(ctx, nodeURL)
	latencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return false, err.Error(), latencyMs, true
	}
	return true, "", latencyMs, true
}

// localAddrCacheTTL bounds how long localInterfaceAddrs() reuses a previous
// net.InterfaceAddrs() result. isLocalNode is invoked on the health-poll hot
// path (once per node every PollIntervalMs, default 2s), so a fresh syscall
// on every single call would add avoidable overhead as node count grows.
// Interfaces changing (DHCP renewal, docker network churn) within this
// window is rare and self-corrects on the next refresh.
const localAddrCacheTTL = 30 * time.Second

var (
	localAddrMu    sync.RWMutex
	localAddrCache map[string]struct{}
	localAddrAt    time.Time
)

// localInterfaceAddrs returns the set of IP addresses (string form) bound to
// this machine's network interfaces, refreshing the cached set at most once
// per localAddrCacheTTL.
func localInterfaceAddrs() map[string]struct{} {
	localAddrMu.RLock()
	if localAddrCache != nil && time.Since(localAddrAt) < localAddrCacheTTL {
		cache := localAddrCache
		localAddrMu.RUnlock()
		return cache
	}
	localAddrMu.RUnlock()

	addrs, err := net.InterfaceAddrs()
	set := make(map[string]struct{}, len(addrs))
	if err == nil {
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				set[ipNet.IP.String()] = struct{}{}
			}
		}
	}

	localAddrMu.Lock()
	localAddrCache = set
	localAddrAt = time.Now()
	localAddrMu.Unlock()
	return set
}

// isLocalNode reports whether a node URL points at the marbor host itself, so that
// local nvidia-smi telemetry may be attributed to it. Remote nodes must not be
// given the marbor host's GPU stats.
func isLocalNode(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	// Fallback: the node may be configured with this machine's actual LAN IP
	// (e.g. 192.168.1.100) rather than a localhost alias. Only compare IP
	// literals against this host's own interface addresses - no DNS
	// resolution here, since that would add latency to a hot-path call for
	// every plain hostname (including genuinely remote ones).
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	_, local := localInterfaceAddrs()[ip.String()]
	return local
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
	n.ConsecutiveSuccesses = 0
	becameUnhealthy := false
	if n.Failures >= r.healthFailureThreshold && n.Healthy {
		n.Healthy = false
		becameUnhealthy = true
		metrics.NodeHealthy(n.Name, 0)
	} else if n.Failures >= r.healthFailureThreshold {
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
