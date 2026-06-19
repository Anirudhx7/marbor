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
)

type ModelInfo struct {
	Name     string `json:"name"`
	SizeVRAM int64  `json:"sizeVram"`
}

type NodeState struct {
	Name         string
	URL          string
	GPUModel     string
	NvidiaIndex  int
	LoadedModels []ModelInfo
	ActiveConns  int32
	Healthy      bool
	LastPollAt   time.Time
	Failures     int
	CPUPercent   float64
	Temperature  *float64
	VRAMTotalMB  int64
	VRAMUsedMB   int64
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
	mu            sync.RWMutex
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
	lastSeen time.Time
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
	// nvidiaCache holds the last nvidia-smi result per GPU index for local nodes.
	// Populated by a separate ticker (nvidiaPollInterval) so that nvidia-smi is
	// never invoked on every /api/ps poll cycle.
	nvidiaCache         map[int]GPUStats
	nvidiaMu            sync.RWMutex
	nvidiaPollInterval  time.Duration
}

func New(cfg config.RoutingConfig, nodesCfg []config.NodeConfig, clouds []config.CloudProvider) *Router {
	nodes := make([]*NodeState, len(nodesCfg))
	for i, n := range nodesCfg {
		nodes[i] = &NodeState{
			Name:              n.Name,
			URL:               n.URL,
			GPUModel:          n.GPUModel,
			NvidiaIndex:       n.NvidiaIndex,
			VRAMTotalMBConfig: n.VRAMTotalMB,
			Healthy:           true,
			FirstSeenAt:       time.Now(),
		}
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
	return &Router{
		nodes:              nodes,
		strategy:           cfg.Strategy,
		fallback:           cfg.Fallback,
		interval:           time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		client:             &http.Client{Timeout: 5 * time.Second},
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
		nvidiaCache:        make(map[int]GPUStats),
		nvidiaPollInterval: nvidiaPollInterval,
	}
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

func (r *Router) SetStrategy(strategy string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strategy = strategy
}

// Strategy returns the current routing strategy.
func (r *Router) Strategy() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.strategy
}

// RouteCloud returns the first enabled cloud provider as fallback when no local nodes are available.
func (r *Router) RouteCloud() *config.CloudProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.clouds {
		if r.clouds[i].Enabled {
			return &r.clouds[i]
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

	seen := make(map[int]bool)
	for _, n := range nodes {
		if !isLocalNode(n.URL) {
			continue
		}
		if seen[n.NvidiaIndex] {
			continue
		}
		seen[n.NvidiaIndex] = true
		gpu, ok := queryGPU(n.NvidiaIndex)
		r.nvidiaMu.Lock()
		if ok {
			r.nvidiaCache[n.NvidiaIndex] = gpu
		} else {
			delete(r.nvidiaCache, n.NvidiaIndex)
		}
		r.nvidiaMu.Unlock()
	}
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
	var wg sync.WaitGroup
	for _, n := range r.nodes {
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
	req, err := http.NewRequestWithContext(ctx, "GET", n.URL+"/api/ps", nil)
	if err != nil {
		r.markFailure(n)
		return
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.markFailure(n)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.markFailure(n)
		return
	}
	// Decode /api/ps. Ollama sends size_vram (snake_case) per loaded model; map it
	// into ModelInfo, which serializes as sizeVram for the admin API. The earlier
	// single-struct approach used the sizeVram tag for decode too, so Ollama's
	// size_vram was silently dropped and per-node used-VRAM was always 0.
	var ps struct {
		Models []struct {
			Name     string `json:"name"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		r.markFailure(n)
		return
	}
	models := make([]ModelInfo, len(ps.Models))
	var psUsedBytes int64
	for i, m := range ps.Models {
		models[i] = ModelInfo{Name: m.Name, SizeVRAM: m.SizeVRAM}
		psUsedBytes += m.SizeVRAM
	}
	psUsedMB := psUsedBytes / (1024 * 1024)

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
func (r *Router) Route(modelName, sessionID string) (*NodeState, bool) {
	if sessionID != "" {
		if node := r.stickyNode(sessionID); node != nil {
			return node, isModelWarm(node, modelName)
		}
	}

	node, warm := r.routeInternal(modelName)
	if node != nil && sessionID != "" {
		r.affinityMu.Lock()
		// Only pin if under the cap — prevents memory-exhaustion DoS from
		// authenticated callers sending unique session IDs at high rate.
		if len(r.affinity) < maxAffinityEntries {
			r.affinity[sessionID] = &affinityEntry{nodeURL: node.URL, lastSeen: time.Now()}
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
	var lastSeen time.Time
	var nodeURL string
	if ok {
		// Copy fields while holding the lock to avoid a data race: the struct
		// is heap-allocated and shared; reading fields after RUnlock is unsafe
		// if sweepAffinity or Route concurrently modifies the same entry.
		lastSeen = entry.lastSeen
		nodeURL = entry.nodeURL
	}
	r.affinityMu.RUnlock()
	if !ok || time.Since(lastSeen) >= r.affinityTTL {
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
		delete(r.affinity, sessionID)
		r.affinityMu.Unlock()
		return nil
	}
	sticky.mu.RLock()
	healthy := sticky.Healthy
	sticky.mu.RUnlock()
	if !healthy {
		r.affinityMu.Lock()
		delete(r.affinity, sessionID)
		r.affinityMu.Unlock()
		return nil
	}

	r.affinityMu.Lock()
	if e, ok := r.affinity[sessionID]; ok {
		e.lastSeen = time.Now()
	}
	r.affinityMu.Unlock()
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
// switch. Callers provide a filtered healthy-node slice.
func (r *Router) routeInternal(modelName string) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	fallback := r.fallback
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		n.mu.RLock()
		isHealthy := n.Healthy
		n.mu.RUnlock()
		if isHealthy {
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
	now := time.Now()
	r.affinityMu.Lock()
	for id, e := range r.affinity {
		if now.Sub(e.lastSeen) >= r.affinityTTL {
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
	node := &NodeState{
		Name:        n.Name,
		URL:         n.URL,
		GPUModel:    n.GPUModel,
		NvidiaIndex: n.NvidiaIndex,
		Healthy:     true,
		FirstSeenAt: time.Now(),
	}
	r.mu.Lock()
	r.nodes = append(r.nodes, node)
	r.mu.Unlock()
	// Start polling immediately
	go r.pollNode(node)
}

func (r *Router) RemoveNode(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, n := range r.nodes {
		if n.Name == name {
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			break
		}
	}
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
func (r *Router) RouteExcluding(modelName string, exclude map[string]bool) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		if exclude[n.URL] {
			continue
		}
		n.mu.RLock()
		isHealthy := n.Healthy
		n.mu.RUnlock()
		if isHealthy {
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

	r.mu.RLock()
	fallback := r.fallback
	r.mu.RUnlock()

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
	}
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

	// This goroutine is the sole fetcher; all others wait on entry.done.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", nodeURL+"/api/tags", nil)
	if err != nil {
		entry.err = fmt.Errorf("build request: %w", err)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		entry.err = fmt.Errorf("fetch tags from %s: %w", nodeURL, err)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		entry.err = fmt.Errorf("tags %s: status %d", nodeURL, resp.StatusCode)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}

	var tagsResp struct {
		Models []TagModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		entry.err = fmt.Errorf("decode tags: %w", err)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}

	entry.models = tagsResp.Models
	r.tagsMu.Lock()
	r.tagsCache[nodeURL] = &TagsCache{
		Models:    tagsResp.Models,
		FetchedAt: time.Now(),
	}
	delete(r.tagsInflight, nodeURL)
	r.tagsMu.Unlock()
	close(entry.done)

	models := make([]TagModel, len(tagsResp.Models))
	copy(models, tagsResp.Models)
	return models, nil
}
