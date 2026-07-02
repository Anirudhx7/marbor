package router

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
	runtimepkg "github.com/ollama-mesh/ollama-mesh/internal/runtime"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

type ModelInfo struct {
	Name     string `json:"name"`
	SizeVRAM int64  `json:"sizeVram"`
}

type NodeState struct {
	Name          string
	URL           string
	GPUModel      string
	NvidiaIndex   int
	LoadedModels  []ModelInfo
	ActiveConns   int32
	RequestsTotal int64 // atomic: lifetime requests routed to this node
	Healthy       bool
	Draining      bool
	LastPollAt    time.Time
	Failures      int
	CPUPercent    float64
	Temperature   *float64
	VRAMTotalMB   int64
	VRAMUsedMB    int64
	// VRAMTotalMBConfig is the operator-declared total VRAM (config vram_total_mb),
	// used for remote nodes nvidia-smi cannot reach. 0 = not declared.
	VRAMTotalMBConfig int64
	// VRAMSource records how VRAM figures were obtained, so the UI never presents
	// a guess as a measurement: "nvidia" (local nvidia-smi), "api" (summed from the
	// node's own /api/ps size_vram), "declared" (total from config), "none".
	VRAMSource    string
	PowerDrawW    float64
	Uptime        string
	HealthHistory []float64
	FirstSeenAt   time.Time
	// Runtime identifies the backend type: "ollama", "vllm", "tgi", "llamacpp".
	// Empty string is treated as "ollama" for backwards compatibility.
	// "auto" means detection is pending; resolved to a real runtime on first poll.
	Runtime    string
	autoDetect bool                    // true if config said runtime: auto; cleared after first detection
	probe      runtimepkg.RuntimeProbe // backend-specific health + warm-model probe
	mu         sync.RWMutex
}

// TagsCache holds a cached result from /api/tags for a single node.
type TagsCache struct {
	Models    []TagModel
	FetchedAt time.Time
}

// TagModel represents one model entry from /api/tags.
type TagModel struct {
	Name string `json:"name"`
	Size int64  `json:"size"` // bytes on disk
}

// tagsInflightEntry represents an in-progress /api/tags fetch.
type tagsInflightEntry struct {
	done   chan struct{}
	models []TagModel
	err    error
}

// maxAffinityEntries is the hard cap on the number of live session-affinity
// entries. When the map is full, new session IDs are routed normally (stateless
// fallback) rather than pinned, to prevent a memory-exhaustion DoS from
// authenticated callers sending unique session IDs at high rate.
const maxAffinityEntries = 10_000

// affinityEntry records which node a session was last routed to and when,
// so the router can honour the sticky-session contract for the TTL window.
type affinityEntry struct {
	nodeURL  string
	lastSeen atomic.Int64 // unix nanoseconds
}

type Router struct {
	nodes          []*NodeState
	strategy       string
	fallback       string
	interval       time.Duration
	client         *http.Client
	mu             sync.RWMutex
	roundRobin     uint32
	rules          []config.RoutingRule
	clouds         []config.CloudProvider
	dockerCfg      config.DockerConfig
	webhookCfg     config.WebhookConfig
	discoveredURLs map[string]struct{} // URLs added via Docker discovery
	// prevHealthy tracks the last known health state per node name for
	// transition detection (healthy -> unhealthy and back).
	prevHealthy map[string]bool
	// tagsCache caches /api/tags results per node URL for 30 seconds.
	tagsCache map[string]*TagsCache
	tagsMu    sync.Mutex
	// tagsInflight prevents concurrent fetches to the same node URL (cache stampede).
	tagsInflight    map[string]*tagsInflightEntry
	upstreamTimeout time.Duration // ResponseHeaderTimeout for upstream Transport
	maxRetries      int           // max alternate nodes to try on upstream failure
	// affinity maps session ID → sticky node. Populated and swept by Route / sweepAffinity.
	affinity    map[string]*affinityEntry
	affinityMu  sync.RWMutex
	affinityTTL time.Duration
	// sessionAffinity gates sticky-session pinning on routing.session_affinity.
	// When false (default), session IDs are ignored and routing is stateless.
	sessionAffinity bool
	// nvidiaCache holds the last nvidia-smi result per GPU index for local nodes.
	// Populated by a separate ticker (nvidiaPollInterval) so that nvidia-smi is
	// never invoked on every /api/ps poll cycle.
	nvidiaCache        map[int]GPUStats
	nvidiaMu           sync.RWMutex
	nvidiaPollInterval time.Duration
	// notifyCh is closed and recreated to broadcast wakes when a connection is freed.
	notifyCh      chan struct{}
	notifyMu      sync.Mutex
	queueDepth    int32 // atomic, current waiters in WaitForNode
	queueMaxDepth int
	queueTimeout  time.Duration
	warmupCfg     config.WarmupConfig
	// nodeWarmup holds per-node runtime warmup settings toggled via the admin API
	// and persisted in the KV store. Merged with warmupCfg by the warm loop.
	// Guarded by r.mu.
	nodeWarmup map[string]NodeWarmup
	// schedules holds recurring time-of-day warmup/drain/undrain actions (guarded
	// by r.mu). schedLastFired (guarded by schedMu) dedupes firing within a minute.
	schedules      []Schedule
	schedMu        sync.Mutex
	schedLastFired map[string]string
	// lastUsed tracks the last-request time per node+model (LRU eviction key),
	// guarded by lruMu (hot path). pinned holds never-evict models per node,
	// guarded by r.mu.
	lastUsed map[string]time.Time
	lruMu    sync.Mutex
	pinned   map[string]map[string]bool
	// lastEvictAt throttles auto-eviction per node (thrash guard), guarded by evictMu.
	evictMu     sync.Mutex
	lastEvictAt map[string]time.Time
	// store persists the warm-state residency map so the router starts warm after
	// a restart instead of cold (Phase 1). Set once via SetStore before Start; nil
	// disables all warm-state persistence (the default for tests). Guarded by r.mu.
	store store.Store
}

// NodeWarmup is the per-node runtime warmup setting: whether proactive warmup is
// enabled for the node and which models to keep resident on it.
type NodeWarmup struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
}

func New(cfg config.RoutingConfig, nodesCfg []config.NodeConfig, clouds []config.CloudProvider) *Router {
	client := &http.Client{Timeout: 5 * time.Second}
	nodes := make([]*NodeState, len(nodesCfg))
	for i, n := range nodesCfg {
		ns := &NodeState{
			Name:              n.Name,
			URL:               n.URL,
			GPUModel:          n.GPUModel,
			NvidiaIndex:       n.NvidiaIndex,
			VRAMTotalMBConfig: n.VRAMTotalMB,
			Healthy:           true,
			FirstSeenAt:       time.Now(),
			Runtime:           n.Runtime,
		}
		if n.Runtime == "auto" {
			ns.autoDetect = true
			// probe is nil until first detection in pollNode
		} else {
			ns.probe = runtimepkg.NewProbe(n.Runtime, client)
		}
		nodes[i] = ns
	}
	cloudsCopy := make([]config.CloudProvider, len(clouds))
	copy(cloudsCopy, clouds)
	prev := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		prev[n.Name] = true // assume healthy at start
	}
	upstreamTimeout := time.Duration(cfg.UpstreamTimeoutMs) * time.Millisecond
	if upstreamTimeout <= 0 {
		upstreamTimeout = 120 * time.Second
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}
	affinityTTL := 10 * time.Minute
	if cfg.SessionAffinityTTL != "" {
		if d, err := time.ParseDuration(cfg.SessionAffinityTTL); err == nil && d > 0 {
			affinityTTL = d
		}
	}
	nvidiaPollInterval := time.Duration(cfg.NvidiaPollIntervalMs) * time.Millisecond
	if nvidiaPollInterval <= 0 {
		nvidiaPollInterval = 30 * time.Second
	}
	// Queue config: 0 means disabled (immediate fallthrough to cloud/503).
	// config.Validate() sets the production defaults (30s, depth 100).
	// Tests that construct RoutingConfig{} directly bypass Validate() and get
	// no queue, which is correct — they test 503/cloud paths, not queuing.
	queueTimeout := time.Duration(cfg.QueueTimeoutMs) * time.Millisecond
	queueMaxDepth := cfg.QueueMaxDepth
	return &Router{
		nodes:              nodes,
		strategy:           cfg.Strategy,
		fallback:           cfg.Fallback,
		interval:           time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		client:             client,
		rules:              cfg.Rules,
		clouds:             cloudsCopy,
		discoveredURLs:     make(map[string]struct{}),
		prevHealthy:        prev,
		tagsCache:          make(map[string]*TagsCache),
		tagsInflight:       make(map[string]*tagsInflightEntry),
		upstreamTimeout:    upstreamTimeout,
		maxRetries:         maxRetries,
		affinity:           make(map[string]*affinityEntry),
		affinityTTL:        affinityTTL,
		sessionAffinity:    cfg.SessionAffinity,
		nodeWarmup:         make(map[string]NodeWarmup),
		schedLastFired:     make(map[string]string),
		lastUsed:           make(map[string]time.Time),
		pinned:             make(map[string]map[string]bool),
		lastEvictAt:        make(map[string]time.Time),
		nvidiaCache:        make(map[int]GPUStats),
		nvidiaPollInterval: nvidiaPollInterval,
		notifyCh:           make(chan struct{}),
		queueMaxDepth:      queueMaxDepth,
		queueTimeout:       queueTimeout,
	}
}

func (r *Router) SetWarmupConfig(cfg config.WarmupConfig) {
	r.mu.Lock()
	r.warmupCfg = cfg
	r.mu.Unlock()
}

// TriggerWarmup fires an immediate warmup ping cycle for all configured models.
// Safe to call concurrently; each (model, node) pair runs in its own goroutine.
func (r *Router) TriggerWarmup(ctx context.Context) {
	go r.pingWarmupModels(ctx)
}

// SetNodeWarmup sets the per-node runtime warmup config (admin-toggled, KV-
// persisted). Disabling with an empty model list removes the node from the warm
// set entirely. Safe for concurrent use.
func (r *Router) SetNodeWarmup(name string, enabled bool, models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodeWarmup == nil {
		r.nodeWarmup = make(map[string]NodeWarmup)
	}
	if !enabled && len(models) == 0 {
		delete(r.nodeWarmup, name)
		return
	}
	cp := append([]string(nil), models...)
	r.nodeWarmup[name] = NodeWarmup{Enabled: enabled, Models: cp}
}

// NodeWarmupSetting returns a copy of the per-node warmup config for name (the
// zero value if unset).
func (r *Router) NodeWarmupSetting(name string) NodeWarmup {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nw := r.nodeWarmup[name]
	return NodeWarmup{Enabled: nw.Enabled, Models: append([]string(nil), nw.Models...)}
}

func (r *Router) SetDockerConfig(cfg config.DockerConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dockerCfg = cfg
}

func (r *Router) SetWebhookConfig(cfg config.WebhookConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webhookCfg = cfg
}

// fireWebhook sends a node-state-change notification to the configured webhook
// URL in a goroutine. Errors are logged but never propagate to the caller.
func (r *Router) fireWebhook(event, nodeName, nodeURL string) {
	r.mu.RLock()
	cfg := r.webhookCfg
	r.mu.RUnlock()
	if !cfg.Enabled || cfg.URL == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[router] panic in goroutine: %v", r)
			}
		}()
		payload := map[string]string{
			"event": event,
			"node":  nodeName,
			"url":   nodeURL,
			"time":  time.Now().UTC().Format(time.RFC3339),
		}
		body, err := json.Marshal(payload)
		if err != nil {
			log.Printf("webhook: marshal payload: %v", err)
			return
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.URL, bytes.NewReader(body))
		if err != nil {
			log.Printf("webhook: create request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.Secret != "" {
			mac := hmac.New(sha256.New, []byte(cfg.Secret))
			mac.Write(body)
			sig := hex.EncodeToString(mac.Sum(nil))
			req.Header.Set("X-Ollama-Mesh-Signature", fmt.Sprintf("sha256=%s", sig))
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("webhook: post %s: %v", strings.TrimRight(cfg.URL, "/"), err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("webhook: server returned %d for event %s", resp.StatusCode, event)
		}
	}()
}

func (r *Router) Rules() []config.RoutingRule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]config.RoutingRule(nil), r.rules...)
}

func (r *Router) AddRule(rule config.RoutingRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
}

func (r *Router) RemoveRule(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, rule := range r.rules {
		if rule.ID == id {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			break
		}
	}
}

func (r *Router) ToggleRule(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, rule := range r.rules {
		if rule.ID == id {
			r.rules[i].Enabled = !r.rules[i].Enabled
			break
		}
	}
}

// validStrategies is the set of accepted routing strategy values.
var validStrategies = map[string]bool{
	"warm-first":        true,
	"least-connections": true,
	"round-robin":       true,
	"vram-aware":        true,
}

func (r *Router) SetStrategy(strategy string) error {
	if !validStrategies[strategy] {
		return fmt.Errorf("unknown routing strategy %q (valid: warm-first, least-connections, round-robin, vram-aware)", strategy)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strategy = strategy
	return nil
}

// Strategy returns the current routing strategy.
func (r *Router) Strategy() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.strategy
}

// RouteCloud returns the first enabled cloud provider as fallback when no local
// nodes are available. It returns a pointer to a *copy* of the provider, never a
// pointer into the r.clouds slice: SetClouds (SIGHUP reload) replaces that slice
// under the write lock, so an aliased pointer could be read concurrently or go
// stale mid-request. The copy is safe to read without holding r.mu.
func (r *Router) RouteCloud() *config.CloudProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.clouds {
		if r.clouds[i].Enabled {
			cp := r.clouds[i]
			return &cp
		}
	}
	return nil
}

func (r *Router) SetClouds(providers []config.CloudProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clouds = make([]config.CloudProvider, len(providers))
	copy(r.clouds, providers)
}

func (n *NodeState) Lock() {
	n.mu.Lock()
}

func (n *NodeState) Unlock() {
	n.mu.Unlock()
}

func (n *NodeState) RLock() {
	n.mu.RLock()
}

func (n *NodeState) RUnlock() {
	n.mu.RUnlock()
}

func (r *Router) Start(ctx context.Context) {
	r.pollNvidiaAll()
	r.pollAll()
	r.discoverAndAddDockerNodes()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	dockerInterval := 30 * time.Second
	r.mu.RLock()
	if r.dockerCfg.PollIntervalMs > 0 {
		dockerInterval = time.Duration(r.dockerCfg.PollIntervalMs) * time.Millisecond
	}
	r.mu.RUnlock()
	dockerTicker := time.NewTicker(dockerInterval)
	defer dockerTicker.Stop()

	nvidiaTicker := time.NewTicker(r.nvidiaPollInterval)
	defer nvidiaTicker.Stop()

	// Sweep expired affinity entries at half the TTL interval to avoid
	// holding memory for sessions that have been idle for >1 TTL.
	sweepInterval := r.affinityTTL / 2
	if sweepInterval < time.Minute {
		sweepInterval = time.Minute
	}
	sweepTicker := time.NewTicker(sweepInterval)
	defer sweepTicker.Stop()

	// Warmup ticker: always runs so per-node warmup toggled at runtime via the
	// admin API (or a scheduled warmup) takes effect without a restart.
	// pingWarmupModels is a fast no-op when there is nothing to warm, and the
	// interval is guarded against zero (config.Validate sets 5m in production;
	// a Router built directly in a test may leave it unset, and NewTicker(0)
	// would panic).
	r.mu.RLock()
	warmupInterval := time.Duration(r.warmupCfg.IntervalMs) * time.Millisecond
	r.mu.RUnlock()
	if warmupInterval <= 0 {
		warmupInterval = 5 * time.Minute
	}
	go r.pingWarmupModels(ctx) // initial ping on startup
	warmupTicker := time.NewTicker(warmupInterval)
	defer warmupTicker.Stop()
	warmupTickerC := warmupTicker.C

	// Schedule ticker: evaluates time-of-day warmup/drain/undrain schedules once
	// a minute (runSchedules dedupes so it fires each schedule at most once per
	// matching minute).
	scheduleTicker := time.NewTicker(1 * time.Minute)
	defer scheduleTicker.Stop()

	// Warm-state flush ticker: Tier 2 of persistence — snapshots the full residency
	// map to SQLite every 60s so drift the immediate lifecycle writes miss (VRAM,
	// last-used) is captured even without a load/unload event.
	warmStateTicker := time.NewTicker(warmStateFlushInterval)
	defer warmStateTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollAll()
		case <-dockerTicker.C:
			r.discoverAndAddDockerNodes()
		case <-nvidiaTicker.C:
			r.pollNvidiaAll()
		case <-sweepTicker.C:
			r.sweepAffinity()
		case <-warmupTickerC:
			go r.pingWarmupModels(ctx)
		case <-scheduleTicker.C:
			r.runSchedules(ctx, time.Now())
		case <-warmStateTicker.C:
			go r.FlushWarmState()
		}
	}
}

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
		detected := runtimepkg.DetectRuntime(ctx, n.URL, r.client)
		n.mu.Lock()
		n.Runtime = detected
		n.probe = runtimepkg.NewProbe(detected, r.client)
		n.autoDetect = false
		n.mu.Unlock()
		log.Printf("auto-detect: node %s resolved to runtime %q", n.Name, detected)
	}

	result, err := n.probe.Probe(ctx, n.URL)
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

	// Fire webhook on recovery (unhealthy -> healthy transition).
	r.mu.Lock()
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
		// Fire webhook on node_down transition.
		r.mu.Lock()
		r.prevHealthy[nodeName] = false
		r.mu.Unlock()
		r.fireWebhook("node_down", nodeName, nodeURL)
	}
}

// Route picks the best healthy node for modelName. If sessionID is non-empty
// and a valid affinity entry exists for it, the previously-used node is
// preferred (KV-cache / context affinity). If the sticky node is gone or
// unhealthy, the entry is evicted and normal warm-first routing applies; the
// new node is then pinned for the session.
//
// runtimeFilter, when non-empty, restricts candidates to nodes whose Runtime
// field matches exactly. Pass "" to allow any runtime (existing behaviour).
func (r *Router) Route(modelName, sessionID, runtimeFilter string) (*NodeState, bool) {
	// Session affinity is opt-in (routing.session_affinity). When disabled,
	// ignore any client-supplied X-Session-ID so routing is fully stateless —
	// no sticky pinning. Previously the session ID was honored unconditionally,
	// so the config flag had no effect.
	if !r.sessionAffinity {
		sessionID = ""
	}
	if sessionID != "" {
		if node := r.stickyNode(sessionID); node != nil {
			// Honour runtime filter even for sticky nodes.
			if runtimeFilter == "" || node.Runtime == runtimeFilter {
				return node, isModelWarm(node, modelName)
			}
			// Sticky node doesn't match filter — evict and re-route.
			r.affinityMu.Lock()
			delete(r.affinity, sessionID)
			r.affinityMu.Unlock()
		}
	}

	node, warm := r.routeInternal(modelName, runtimeFilter)
	if node != nil && sessionID != "" {
		r.affinityMu.Lock()
		// Only pin if under the cap — prevents memory-exhaustion DoS from
		// authenticated callers sending unique session IDs at high rate.
		if len(r.affinity) < maxAffinityEntries {
			entry := &affinityEntry{nodeURL: node.URL}
			entry.lastSeen.Store(time.Now().UnixNano())
			r.affinity[sessionID] = entry
		}
		r.affinityMu.Unlock()
	}
	return node, warm
}

// stickyNode returns the pinned node for sessionID if it is still healthy and
// within the TTL window, refreshing the TTL on success. Returns nil to signal
// "fall through to normal routing."
func (r *Router) stickyNode(sessionID string) *NodeState {
	r.affinityMu.RLock()
	entry, ok := r.affinity[sessionID]
	var lastSeenNano int64
	var nodeURL string
	if ok {
		// Copy fields while holding the lock to avoid a data race: the struct
		// is heap-allocated and shared; reading fields after RUnlock is unsafe
		// if sweepAffinity or Route concurrently modifies the same entry.
		lastSeenNano = entry.lastSeen.Load()
		nodeURL = entry.nodeURL
	}
	r.affinityMu.RUnlock()
	if !ok || time.Since(time.Unix(0, lastSeenNano)) >= r.affinityTTL {
		return nil
	}

	r.mu.RLock()
	var sticky *NodeState
	for _, n := range r.nodes {
		if n.URL == nodeURL {
			sticky = n
			break
		}
	}
	r.mu.RUnlock()

	if sticky == nil {
		r.affinityMu.Lock()
		if e, ok := r.affinity[sessionID]; ok && time.Since(time.Unix(0, e.lastSeen.Load())) >= r.affinityTTL {
			delete(r.affinity, sessionID)
		}
		r.affinityMu.Unlock()
		return nil
	}
	sticky.mu.RLock()
	healthy := sticky.Healthy
	draining := sticky.Draining
	sticky.mu.RUnlock()
	if !healthy || draining {
		r.affinityMu.Lock()
		if e, ok := r.affinity[sessionID]; ok && time.Since(time.Unix(0, e.lastSeen.Load())) >= r.affinityTTL {
			delete(r.affinity, sessionID)
		}
		r.affinityMu.Unlock()
		return nil
	}

	r.affinityMu.RLock()
	if e, ok := r.affinity[sessionID]; ok {
		e.lastSeen.Store(time.Now().UnixNano())
	}
	r.affinityMu.RUnlock()
	return sticky
}

// isModelWarm reports whether modelName is currently loaded in VRAM on node n.
func isModelWarm(n *NodeState, modelName string) bool {
	if modelName == "" {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, m := range n.LoadedModels {
		if m.Name == modelName {
			return true
		}
	}
	return false
}

// routeInternal is the core warm-first / fallback routing logic, extracted so
// both Route and RouteExcluding can share it without duplicating the strategy
// switch. runtimeFilter, when non-empty, restricts candidates to nodes whose
// Runtime matches exactly.
func (r *Router) routeInternal(modelName, runtimeFilter string) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	fallback := r.fallback
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		if runtimeFilter != "" && n.Runtime != runtimeFilter {
			continue // skip nodes that don't match the requested runtime
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		n.mu.RUnlock()
		if isHealthy && !isDraining {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return nil, false
	}

	if modelName != "" && strategy == "warm-first" {
		var warm []*NodeState
		for _, n := range healthy {
			n.mu.RLock()
			for _, m := range n.LoadedModels {
				if m.Name == modelName {
					warm = append(warm, n)
					break
				}
			}
			n.mu.RUnlock()
		}
		if len(warm) > 0 {
			metrics.CacheHit()
			return pickLeastConns(warm), true
		}
		metrics.CacheMiss()
	}

	switch fallback {
	case "vram-aware":
		return pickMostFreeVRAM(healthy), false
	case "round-robin":
		idx := atomic.AddUint32(&r.roundRobin, 1) % uint32(len(healthy))
		return healthy[idx], false
	default: // "least-connections" or ""
		return pickLeastConns(healthy), false
	}
}

// sweepAffinity removes expired session-affinity entries. Called periodically
// from Start to bound memory usage on long-running deployments.
func (r *Router) sweepAffinity() {
	now := time.Now().UnixNano()
	r.affinityMu.Lock()
	for id, e := range r.affinity {
		if now-e.lastSeen.Load() >= int64(r.affinityTTL) {
			delete(r.affinity, id)
		}
	}
	r.affinityMu.Unlock()
}

func pickLeastConns(nodes []*NodeState) *NodeState {
	var best *NodeState
	minConns := int32(math.MaxInt32)
	for _, n := range nodes {
		conns := atomic.LoadInt32(&n.ActiveConns)
		if conns < minConns {
			minConns = conns
			best = n
		}
	}
	return best
}

// pickMostFreeVRAM selects the healthy node with the most free VRAM.
// Nodes where VRAMTotalMB == 0 (unknown capacity) or VRAMUsedMB >= VRAMTotalMB
// (overcommitted / at capacity) are excluded; if ALL eligible nodes are excluded
// it falls back to pickLeastConns so a request is never dropped.
func pickMostFreeVRAM(nodes []*NodeState) *NodeState {
	var best *NodeState
	var bestFree int64 = 0
	for _, n := range nodes {
		n.mu.RLock()
		total := n.VRAMTotalMB
		used := n.VRAMUsedMB
		n.mu.RUnlock()
		if total <= 0 {
			continue // capacity unknown
		}
		free := total - used
		if free <= 0 {
			continue // at or over capacity
		}
		if free > bestFree {
			bestFree = free
			best = n
		}
	}
	if best == nil {
		return pickLeastConns(nodes) // all unknown/full: safe degradation
	}
	return best
}

func (r *Router) AddNode(n config.NodeConfig) {
	// Defense-in-depth: reject invalid or link-local/metadata node URLs even when
	// they arrive from the store overlay or Docker discovery, which bypass
	// config.Validate. Prevents an SSRF relay from a persisted/discovered node.
	if err := config.ValidateNodeURL(n.URL); err != nil {
		log.Printf("router: rejecting node %q: %v", n.Name, err)
		return
	}
	node := &NodeState{
		Name:        n.Name,
		URL:         n.URL,
		GPUModel:    n.GPUModel,
		NvidiaIndex: n.NvidiaIndex,
		Healthy:     true,
		FirstSeenAt: time.Now(),
		Runtime:     n.Runtime,
	}
	if n.Runtime == "auto" {
		node.autoDetect = true
		// probe is nil until first detection in pollNode
	} else {
		node.probe = runtimepkg.NewProbe(n.Runtime, r.client)
	}
	r.mu.Lock()
	r.nodes = append(r.nodes, node)
	r.mu.Unlock()
	// Start polling immediately
	go r.pollNode(node)
}

func (r *Router) RemoveNode(name string) {
	r.mu.Lock()
	var urlToRemove string
	for i, n := range r.nodes {
		if n.Name == name {
			urlToRemove = n.URL
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			break
		}
	}
	delete(r.prevHealthy, name)
	if urlToRemove != "" {
		delete(r.discoveredURLs, urlToRemove)
		r.tagsMu.Lock()
		delete(r.tagsCache, urlToRemove)
		r.tagsMu.Unlock()
	}
	st := r.store
	r.mu.Unlock()

	// Drop the removed node's warm state immediately (Tier 1): its residency is
	// no longer meaningful and must not be restored on the next start.
	if st != nil {
		if err := st.DeleteWarmStateByNode(name); err != nil {
			log.Printf("warmstate: delete node %s: %v", name, err)
		}
	}
}

// SyncNodes reconciles the live node pool against a new config slice.
// Nodes present in the config but not in the current pool are added.
// Nodes in the current pool but absent from the config are removed.
// Nodes present in both (matched by name AND URL) are left untouched so
// in-flight connections and health history are not disrupted.
func (r *Router) SyncNodes(newNodes []config.NodeConfig) (added, removed int) {
	// Build lookup sets.
	newByName := make(map[string]config.NodeConfig, len(newNodes))
	for _, n := range newNodes {
		newByName[n.Name] = n
	}

	r.mu.RLock()
	currentNames := make(map[string]string, len(r.nodes)) // name -> URL
	for _, n := range r.nodes {
		currentNames[n.Name] = n.URL
	}
	r.mu.RUnlock()

	// Remove nodes that are no longer in config.
	for name := range currentNames {
		if _, ok := newByName[name]; !ok {
			r.RemoveNode(name)
			removed++
		}
	}

	// Add nodes that are new in config.
	for _, n := range newNodes {
		if existingURL, ok := currentNames[n.Name]; !ok || existingURL != n.URL {
			if ok {
				// Same name, different URL — remove old first.
				r.RemoveNode(n.Name)
				removed++
			}
			r.AddNode(n)
			added++
		}
	}
	return added, removed
}

// DrainNode marks a node as draining: it will no longer receive new requests
// but in-flight connections are allowed to finish. Returns false if not found.
func (r *Router) DrainNode(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == name {
			n.mu.Lock()
			n.Draining = true
			nodeURL := n.URL
			n.mu.Unlock()
			r.fireWebhook("node_drain", name, nodeURL)
			return true
		}
	}
	return false
}

// UndrainNode clears the draining flag, returning the node to the active pool.
// Returns false if not found.
func (r *Router) UndrainNode(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == name {
			n.mu.Lock()
			n.Draining = false
			nodeURL := n.URL
			n.mu.Unlock()
			r.fireWebhook("node_undrain", name, nodeURL)
			return true
		}
	}
	return false
}

// NodePatch holds optional runtime-mutable node metadata.
// Only non-nil fields are applied.
type NodePatch struct {
	VRAMTotalMB *int64  `json:"vram_total_mb"`
	GPUModel    *string `json:"gpu_model"`
}

// PatchNode applies runtime metadata overrides to a node without restarting.
// Returns false if the node is not found.
func (r *Router) PatchNode(name string, patch NodePatch) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == name {
			n.mu.Lock()
			if patch.VRAMTotalMB != nil {
				n.VRAMTotalMBConfig = *patch.VRAMTotalMB
				// Only override live total when nvidia-smi has no measurement.
				if n.VRAMSource == "none" || n.VRAMSource == "declared" {
					n.VRAMTotalMB = *patch.VRAMTotalMB
					n.VRAMSource = "declared"
				}
			}
			if patch.GPUModel != nil {
				n.GPUModel = *patch.GPUModel
			}
			n.mu.Unlock()
			return true
		}
	}
	return false
}

func (r *Router) Nodes() []*NodeState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*NodeState, len(r.nodes))
	copy(out, r.nodes)
	return out
}

// NodeURLs returns a map of node name to URL for all configured nodes.
func (r *Router) NodeURLs() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m := make(map[string]string, len(r.nodes))
	for _, s := range r.nodes {
		m[s.Name] = s.URL
	}
	return m
}

// UpstreamTimeout returns the configured upstream response-header timeout.
// Used by proxy.go to set Transport.ResponseHeaderTimeout without changing
// NewHandler's signature.
func (r *Router) UpstreamTimeout() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// RoutingConfig is not stored on Router directly; read from the nodes
	// slice is not applicable. We store the timeout in a field set during New.
	return r.upstreamTimeout
}

// MaxRetries returns the configured maximum number of alternate nodes to try
// on upstream failure before falling back to cloud or returning 502.
func (r *Router) MaxRetries() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.maxRetries
}

// RouteExcluding picks the best healthy node for modelName, excluding any
// node whose URL appears in the exclude map. Used by the retry loop in
// proxy.go to avoid re-selecting an already-failed node.
//
// runtimeFilter, when non-empty, additionally restricts candidates to nodes
// whose Runtime field matches exactly. Pass "" to allow any runtime.
func (r *Router) RouteExcluding(modelName, runtimeFilter string, exclude map[string]bool) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	fallback := r.fallback // read here to avoid a second lock acquisition (#17)
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		if exclude[n.URL] {
			continue
		}
		if runtimeFilter != "" && n.Runtime != runtimeFilter {
			continue // skip nodes that don't match the requested runtime
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		n.mu.RUnlock()
		if isHealthy && !isDraining {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return nil, false
	}

	if modelName != "" && strategy == "warm-first" {
		var warm []*NodeState
		for _, n := range healthy {
			n.mu.RLock()
			for _, m := range n.LoadedModels {
				if m.Name == modelName {
					warm = append(warm, n)
					break
				}
			}
			n.mu.RUnlock()
		}
		if len(warm) > 0 {
			return pickLeastConns(warm), true
		}
	}

	switch fallback {
	case "vram-aware":
		return pickMostFreeVRAM(healthy), false
	case "round-robin":
		idx := atomic.AddUint32(&r.roundRobin, 1) % uint32(len(healthy))
		return healthy[idx], false
	default: // "least-connections" or ""
		return pickLeastConns(healthy), false
	}
}

func (r *Router) IncrConn(node *NodeState) {
	if node != nil {
		v := atomic.AddInt32(&node.ActiveConns, 1)
		atomic.AddInt64(&node.RequestsTotal, 1)
		metrics.ActiveConnections(node.Name, float64(v))
	}
}

func (r *Router) DecrConn(node *NodeState) {
	if node != nil {
		v := atomic.AddInt32(&node.ActiveConns, -1)
		if v < 0 {
			atomic.StoreInt32(&node.ActiveConns, 0)
			v = 0
		}
		metrics.ActiveConnections(node.Name, float64(v))
		// Wake up all WaitForNode callers — a slot just freed.
		r.notifyMu.Lock()
		ch := r.notifyCh
		r.notifyCh = make(chan struct{})
		close(ch)
		r.notifyMu.Unlock()
	}
}

// WaitForNode is the queued variant of Route. It first tries Route() immediately;
// if no node is available it waits up to queueTimeout for one to free up (signaled
// by DecrConn). Returns nil after timeout or context cancellation, at which point
// the caller should fall through to cloud fallback or 503.
//
// runtimeFilter, when non-empty, restricts candidates to nodes whose Runtime
// matches exactly (e.g. "ollama"). Pass "" to allow any runtime.
//
// If the queue is already at queueMaxDepth, returns nil immediately without queuing
// to prevent unbounded memory growth under sustained overload.
func (r *Router) WaitForNode(ctx context.Context, modelName, sessionID, runtimeFilter string) (*NodeState, bool) {
	// Fast path: immediate route.
	if node, warm := r.Route(modelName, sessionID, runtimeFilter); node != nil {
		return node, warm
	}

	// Queue disabled (timeout or depth == 0): fall through immediately.
	// config.Validate() sets the production defaults; callers that bypass
	// Validate() (unit tests, zero-config New()) get no queue.
	if r.queueTimeout <= 0 || r.queueMaxDepth <= 0 {
		return nil, false
	}

	// Claim a queue slot atomically. Reject if already at capacity.
	depth := atomic.AddInt32(&r.queueDepth, 1)
	if int(depth) > r.queueMaxDepth {
		atomic.AddInt32(&r.queueDepth, -1)
		return nil, false
	}
	metrics.QueueDepth(float64(depth))
	defer func() {
		d := atomic.AddInt32(&r.queueDepth, -1)
		metrics.QueueDepth(float64(d))
	}()

	timer := time.NewTimer(r.queueTimeout)
	defer timer.Stop()
	// Periodic safety-net retry: coalesced notifyCh signals can miss concurrent
	// DecrConn bursts (channel capacity 1). 500ms poll is the fallback safety net;
	// immediate wakeups are handled via the notifyCh channel (#15).
	retryTick := time.NewTicker(500 * time.Millisecond)
	defer retryTick.Stop()

	for {
		r.notifyMu.Lock()
		ch := r.notifyCh
		r.notifyMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, false
		case <-timer.C:
			metrics.QueueTimeout()
			return nil, false
		case <-ch:
			if node, warm := r.Route(modelName, sessionID, runtimeFilter); node != nil {
				return node, warm
			}
		case <-retryTick.C:
			if node, warm := r.Route(modelName, sessionID, runtimeFilter); node != nil {
				return node, warm
			}
		}
	}
}

// QueueDepth returns the current number of requests waiting in WaitForNode.
func (r *Router) QueueDepth() int {
	return int(atomic.LoadInt32(&r.queueDepth))
}

func ExtractModelName(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err == nil && req.Model != "" {
		return req.Model
	}
	return ""
}

// FetchModelTags fetches /api/tags from a node and returns the list of
// downloaded models with their disk sizes. Results are cached for 30 seconds
// to avoid hammering nodes on every dashboard refresh.
// Concurrent callers for the same nodeURL share a single in-flight request
// (singleflight pattern) to prevent cache stampedes.
func (r *Router) FetchModelTags(nodeURL string) ([]TagModel, error) {
	const cacheTTL = 30 * time.Second

	r.tagsMu.Lock()
	// Serve from cache if fresh.
	if cached, ok := r.tagsCache[nodeURL]; ok && time.Since(cached.FetchedAt) < cacheTTL {
		models := make([]TagModel, len(cached.Models))
		copy(models, cached.Models)
		r.tagsMu.Unlock()
		return models, nil
	}
	// Deduplicate concurrent fetches to the same node.
	if inflight, ok := r.tagsInflight[nodeURL]; ok {
		r.tagsMu.Unlock()
		<-inflight.done
		if inflight.err != nil {
			return nil, inflight.err
		}
		models := make([]TagModel, len(inflight.models))
		copy(models, inflight.models)
		return models, nil
	}
	entry := &tagsInflightEntry{done: make(chan struct{})}
	r.tagsInflight[nodeURL] = entry
	r.tagsMu.Unlock()

	// Panic safety: always close entry.done so waiters are never blocked forever
	// even if a panic occurs mid-fetch (#14).
	defer func() {
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		// close is idempotent-safe here because we only reach this defer once;
		// the channel is unbuffered and only this path closes it.
		select {
		case <-entry.done:
			// already closed (shouldn't happen, but guard against double-close)
		default:
			close(entry.done)
		}
	}()

	// This function is the sole fetcher; all others wait on entry.done.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", nodeURL+"/api/tags", nil)
	if err != nil {
		entry.err = fmt.Errorf("build request: %w", err)
		return nil, entry.err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		entry.err = fmt.Errorf("fetch tags from %s: %w", nodeURL, err)
		return nil, entry.err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		entry.err = fmt.Errorf("tags %s: status %d", nodeURL, resp.StatusCode)
		return nil, entry.err
	}

	var tagsResp struct {
		Models []TagModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		entry.err = fmt.Errorf("decode tags: %w", err)
		return nil, entry.err
	}

	entry.models = tagsResp.Models
	r.tagsMu.Lock()
	r.tagsCache[nodeURL] = &TagsCache{
		Models:    tagsResp.Models,
		FetchedAt: time.Now(),
	}
	r.tagsMu.Unlock()

	models := make([]TagModel, len(tagsResp.Models))
	copy(models, tagsResp.Models)
	return models, nil
}
