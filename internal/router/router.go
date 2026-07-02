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
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
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
	Runtime        string
	autoDetect     bool                    // true if config said runtime: auto; cleared after first detection
	probe          runtimepkg.RuntimeProbe // backend-specific health + warm-model probe
	LastErrorAt    time.Time
	SuccessHistory []bool
	mu             sync.RWMutex
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

// RecordRequestOutcome logs whether a request routed to a node succeeded or failed.
// Failure marks LastErrorAt, which triggers node cooldown.
func (r *Router) RecordRequestOutcome(nodeName string, success bool) {
	n := r.findNode(nodeName)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.SuccessHistory = append(n.SuccessHistory, success)
	if len(n.SuccessHistory) > 50 {
		n.SuccessHistory = n.SuccessHistory[len(n.SuccessHistory)-50:]
	}
	if !success {
		n.LastErrorAt = time.Now()
	}
}
