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
	"sort"
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
	ColdStarts    int64 // atomic
	WarmHits      int64 // atomic
	TokensTotal   int64 // atomic
	LatencySumMs  int64 // atomic
	LatencyCount  int64 // atomic
	Healthy       bool
	Draining      bool
	// DrainedReason records why Draining was set (e.g. "manual", "thermal") -
	// persisted and restored alongside Draining, surfaced to operators in the
	// UI so they can tell an admin-initiated drain from a watchdog-triggered
	// one.
	DrainedReason string
	// PrewarmDisabled is a live, admin-toggleable, in-memory-only flag: when
	// true, the predictive engine skips this node for new warmup triggers.
	// Never persisted - it always reverts to false (prewarm enabled) on
	// restart, unlike Draining which is an operational state the admin
	// deliberately sets and expects to persist across a reload.
	PrewarmDisabled bool
	// ThermalBreaches counts consecutive polls at/above the configured
	// thermal_watchdog threshold; reset to 0 on any poll below it. Drives
	// Sustained Degradation Auto-Drain.
	ThermalBreaches int
	LastPollAt      time.Time
	Failures        int
	// ConsecutiveSuccesses counts successful polls in a row while the node is
	// unhealthy, gating the unhealthy->healthy transition (flapping
	// hysteresis). Reset to 0 on any failure; irrelevant once Healthy is true.
	ConsecutiveSuccesses int
	CPUPercent           float64
	Temperature          *float64
	VRAMTotalMB          int64
	VRAMUsedMB           int64
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
	// Runtime identifies the backend type: "ollama", "vllm", "tgi", "llamacpp", "mlx".
	// Empty string is treated as "ollama" for backwards compatibility.
	// "auto" means detection is pending; resolved to a real runtime on first poll.
	Runtime string
	// VRAMOverrides is the operator-declared, per-model VRAM size (MB) from
	// config (NodeConfig.VRAMOverrides). Used by estimateModelSizeBytes as a
	// fallback when a node's runtime API can't report a real observed size
	// (non-Ollama backends). Set once at construction; never mutated at
	// runtime, so it is safe to read under RLock like any other field.
	VRAMOverrides  map[string]int64
	autoDetect     bool                    // true if config said runtime: auto; cleared after first detection
	probe          runtimepkg.RuntimeProbe // backend-specific health + runtime warm-model probe
	LastErrorAt    time.Time
	SuccessHistory []bool

	// Node Agent-derived telemetry (see internal/nodeagent, .local/specs/node-agent.md).
	// AgentPresent is true only after a successful poll of this node's agent
	// on the most recent poll cycle; it is set back to false on any failure
	// or when no agent is configured, so a stale AgentVersion/FanPercent
	// value is never displayed as current (R1). AgentVersion is the agent
	// binary's reported build version. FanPercent/RAMUsedMB/DiskFreeGB are
	// only meaningful when AgentPresent is true - consumers must check the
	// flag rather than treating a zero value as a measurement.
	AgentPresent bool
	AgentVersion string
	FanPercent   *float64
	RAMUsedMB    int64
	DiskFreeGB   float64
	// AgentCapabilities lists what the polled agent build actually supports
	// (e.g. "telemetry") - the mesh/UI must gate any agent-dependent feature
	// on this list rather than assuming every agent supports everything,
	// since a fleet naturally has agents on different builds over time.
	// AgentPlatform/AgentArchitecture/AgentGPUVendor/AgentRuntime are
	// self-reported agent metadata (runtime.GOOS/GOARCH, selected GPU
	// backend, locally-detected inference runtime) surfaced for debugging a
	// mixed-version/mixed-vendor/mixed-runtime fleet - all cleared alongside
	// AgentPresent so a disabled/unreachable agent never displays stale
	// metadata as current.
	AgentCapabilities []string
	AgentPlatform     string
	AgentArchitecture string
	AgentGPUVendor    string
	AgentRuntime      string
	// agentSchemaWarned latches once a poll observes an agent reporting a
	// schema_version newer than this mesh binary's own nodeagent.SchemaVersion
	// - logged once per node (not every poll cycle) purely for operator
	// visibility during a rolling upgrade where an agent got updated ahead of
	// the mesh. Decoding itself never depends on this - see agent_poll.go.
	agentSchemaWarned bool

	mu sync.RWMutex
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
	liteLLM        config.LiteLLMConfig
	dockerCfg      config.DockerConfig
	webhookCfg     config.WebhookConfig
	discoveredURLs map[string]struct{} // URLs added via Docker discovery
	// prevHealthy tracks the last known health state per node name for
	// transition detection (healthy -> unhealthy and back).
	prevHealthy map[string]bool
	// prevAgentPresent mirrors prevHealthy for Node Agent reachability, so an
	// operator gets an agent_down/agent_up webhook when a configured agent
	// stops/resumes responding - independent of the node's own inference
	// runtime health, which pollNode/markFailure already cover separately.
	prevAgentPresent map[string]bool
	// tagsCache caches /api/tags results per node URL for 30 seconds.
	tagsCache map[string]*TagsCache
	tagsMu    sync.Mutex
	// tagsInflight prevents concurrent fetches to the same node URL (cache stampede).
	tagsInflight    map[string]*tagsInflightEntry
	upstreamTimeout time.Duration // ResponseHeaderTimeout for upstream Transport
	maxRetries      int           // max alternate nodes to try on upstream failure
	// healthFailureThreshold/healthSuccessThreshold are the asymmetric
	// consecutive-poll thresholds for the healthy<->unhealthy transition
	// (flapping hysteresis). Immutable after construction.
	healthFailureThreshold int
	healthSuccessThreshold int
	// fallbackChains maps a model to an ordered list of already-downloaded
	// alternates to try when the primary model doesn't fit anywhere. Opt-in,
	// immutable after construction (config-only, not runtime-toggleable).
	fallbackChains map[string][]string
	// overflowSLA, when > 0, caps how long WaitForNode waits in the local
	// capacity queue before returning nil (triggering cloud fallback or 503)
	// - see config.RoutingConfig.OverflowSLAMs. It never affects Route()'s
	// Hard-Constraint filtering, only how long a request waits for capacity.
	overflowSLA time.Duration
	// thermalWatchdog gates Sustained Degradation Auto-Drain. Immutable after
	// construction (config-only, not runtime-toggleable).
	thermalWatchdog config.ThermalWatchdogConfig
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
	timezone      string // timezone location name (e.g. "UTC", "Asia/Kolkata", "Local")
	warmupCfg     config.WarmupConfig
	// nodeWarmup holds per-node runtime warmup settings toggled via the admin API
	// and persisted in the KV store. Merged with warmupCfg by the warm loop.
	// Guarded by r.mu.
	nodeWarmup map[string]NodeWarmup
	// nodeAgents holds per-node Node Agent poll configuration (enabled, port,
	// bearer token), toggled via the admin API and persisted in the
	// node_agent table (internal/store). Guarded by r.mu, same pattern as
	// nodeWarmup. A node absent from this map (or present with Enabled:
	// false) is polled for /api/ps as normal but never has its agent fields
	// (AgentPresent, FanPercent, RAMUsedMB, DiskFreeGB) populated.
	nodeAgents map[string]NodeAgentConfig
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
	// warmReserved tracks VRAM bytes reserved for warmup loads that have started
	// but aren't yet reflected in a node's LoadedModels (populated only by the
	// next /api/ps poll). Keyed by node -> model. Guarded by evictMu. See
	// reserveWarmBytes in eviction.go for why this exists.
	warmReserved map[string]map[string]warmReservation
	// warmPriority ranks the "keep warm" set per node (0 = highest priority,
	// i.e. first in the configured/toggled list). Refreshed every
	// pingWarmupModels tick from the current config+toggle order. Used by
	// EvictForHeadroom so that when two or more keep-warm models don't fit
	// together, the same higher-priority model always wins - deterministically,
	// not whichever happened to warm last. Guarded by warmPriorityMu.
	warmPriorityMu sync.RWMutex
	warmPriority   map[string]map[string]int
	// store persists the warm-state residency map so the router starts warm after
	// a restart instead of cold (Phase 1). Set once via SetStore before Start; nil
	// disables all warm-state persistence (the default for tests). Guarded by r.mu.
	store store.Store

	// Predictive prewarming fields (Step 5)
	predictiveMu             sync.Mutex
	predictiveHistory        []TransitionEntry
	lastModelRequested       string
	activePredictions        []ActivePrediction
	predictionsMadeTotal     int64
	predictionsMetTotal      int64
	lastAccuracyLogAt        time.Time
	lastTimeOfDayPrewarmHour int
	// decisionLog is a capped ring buffer of the most recent predictive
	// decisions, exposed read-only via GET /api/predictive/decisions for
	// dashboard visibility. The engine has no scheduled queue to inspect -
	// it's a stateless tick-and-act loop - so this is the log of what it
	// actually decided on each tick, not a plan of what it will do next.
	decisionLog []PredictiveDecision

	// pollInFlight guards against overlapping pollAll cycles: if a node hangs
	// past its 5s timeout, the next ticker tick skips rather than stacking a
	// second concurrent pollAll goroutine.
	pollInFlight atomic.Bool
}

// NodeWarmup is the per-node runtime warmup setting: whether proactive warmup is
// enabled for the node and which models to keep resident on it.
type NodeWarmup struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
}

// sortCloudsByPriority orders providers highest-priority-first, in place.
// Stable so equal-priority providers (the common case: priority left at its
// zero-value default) keep their original insertion order, matching this
// package's existing "first enabled" behavior when no priority is set.
func sortCloudsByPriority(clouds []config.CloudProvider) {
	sort.SliceStable(clouds, func(i, j int) bool {
		return clouds[i].Priority > clouds[j].Priority
	})
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
			VRAMOverrides:     n.VRAMOverrides,
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
	sortCloudsByPriority(cloudsCopy)
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
	// no queue, which is correct - they test 503/cloud paths, not queuing.
	queueTimeout := time.Duration(cfg.QueueTimeoutMs) * time.Millisecond
	queueMaxDepth := cfg.QueueMaxDepth
	healthFailureThreshold := cfg.HealthFailureThreshold
	if healthFailureThreshold <= 0 {
		healthFailureThreshold = 3
	}
	healthSuccessThreshold := cfg.HealthSuccessThreshold
	if healthSuccessThreshold <= 0 {
		healthSuccessThreshold = 2
	}
	thermalWatchdog := cfg.ThermalWatchdog
	if thermalWatchdog.Enabled && thermalWatchdog.ConsecutiveBreaches <= 0 {
		thermalWatchdog.ConsecutiveBreaches = 3
	}
	return &Router{
		nodes:                    nodes,
		strategy:                 cfg.Strategy,
		fallback:                 cfg.Fallback,
		interval:                 time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		client:                   client,
		rules:                    cfg.Rules,
		clouds:                   cloudsCopy,
		discoveredURLs:           make(map[string]struct{}),
		prevHealthy:              prev,
		prevAgentPresent:         make(map[string]bool),
		tagsCache:                make(map[string]*TagsCache),
		tagsInflight:             make(map[string]*tagsInflightEntry),
		upstreamTimeout:          upstreamTimeout,
		maxRetries:               maxRetries,
		healthFailureThreshold:   healthFailureThreshold,
		healthSuccessThreshold:   healthSuccessThreshold,
		fallbackChains:           cfg.FallbackChains,
		overflowSLA:              time.Duration(cfg.OverflowSLAMs) * time.Millisecond,
		thermalWatchdog:          thermalWatchdog,
		affinity:                 make(map[string]*affinityEntry),
		affinityTTL:              affinityTTL,
		sessionAffinity:          cfg.SessionAffinity,
		nodeWarmup:               make(map[string]NodeWarmup),
		nodeAgents:               make(map[string]NodeAgentConfig),
		schedLastFired:           make(map[string]string),
		lastUsed:                 make(map[string]time.Time),
		pinned:                   make(map[string]map[string]bool),
		lastEvictAt:              make(map[string]time.Time),
		warmReserved:             make(map[string]map[string]warmReservation),
		nvidiaCache:              make(map[int]GPUStats),
		nvidiaPollInterval:       nvidiaPollInterval,
		notifyCh:                 make(chan struct{}),
		queueMaxDepth:            queueMaxDepth,
		queueTimeout:             queueTimeout,
		lastAccuracyLogAt:        time.Now(),
		lastTimeOfDayPrewarmHour: -1,
	}
}

func (r *Router) SetWarmupConfig(cfg config.WarmupConfig) {
	r.mu.Lock()
	r.warmupCfg = cfg
	r.mu.Unlock()
}

func (r *Router) SetTimezone(tz string) {
	r.mu.Lock()
	r.timezone = tz
	r.mu.Unlock()
}

func (r *Router) Timezone() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.timezone
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

// NodeAgentConfig is the router's in-memory view of a node's Node Agent
// poll configuration: whether the agent is enabled, which port it listens
// on, and the bearer token the mesh presents when polling it.
type NodeAgentConfig struct {
	Enabled bool
	Port    int
	Token   string
}

// SetNodeAgent sets the per-node Node Agent poll config (admin-toggled,
// store-persisted by the caller). Disabling removes the node from the map
// entirely so pollAgentTelemetry's "no agent configured" branch runs on the
// next poll, clearing any previously-reported agent fields.
func (r *Router) SetNodeAgent(name string, enabled bool, port int, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodeAgents == nil {
		r.nodeAgents = make(map[string]NodeAgentConfig)
	}
	if !enabled {
		delete(r.nodeAgents, name)
		return
	}
	r.nodeAgents[name] = NodeAgentConfig{Enabled: true, Port: port, Token: token}
}

// NodeAgentSetting returns the agent config for name and whether one is
// configured (enabled) at all.
func (r *Router) NodeAgentSetting(name string) (NodeAgentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.nodeAgents[name]
	return cfg, ok
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
	sortCloudsByPriority(r.clouds)
}

// SetLiteLLM updates the LiteLLM integration config. When Enabled, CloudChain
// ignores the per-provider list entirely and routes cloud fallback through
// the single LiteLLM endpoint instead - LiteLLM owns provider ordering and
// retries internally once a request reaches it, so running both this
// package's priority chain and LiteLLM's own chain at the same time would
// double-manage failover.
func (r *Router) SetLiteLLM(cfg config.LiteLLMConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liteLLM = cfg
}

// CloudChain returns every enabled cloud provider as a priority-ordered
// snapshot for cloud fallback, or - when LiteLLM integration is enabled - a
// single synthetic provider pointing at the LiteLLM endpoint. Every element
// is a copy, never an alias into r.clouds, matching the invariant RouteCloud
// documents above: SetClouds/SetLiteLLM can replace the backing state under
// the write lock at any time, and a caller mid-request must not observe that.
func (r *Router) CloudChain() []config.CloudProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.liteLLM.Enabled && r.liteLLM.URL != "" {
		return []config.CloudProvider{{
			Name:     "litellm",
			Provider: "openai",
			BaseURL:  r.liteLLM.URL,
			APIKey:   r.liteLLM.APIKey,
			Enabled:  true,
		}}
	}
	out := make([]config.CloudProvider, 0, len(r.clouds))
	for _, cp := range r.clouds {
		if cp.Enabled {
			out = append(out, cp)
		}
	}
	return out
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

// GetRuntime returns n.Runtime under RLock. Runtime is written under Lock by
// pollNode's auto-detect path, so every read site must go through this
// accessor rather than a bare field read.
func (n *NodeState) GetRuntime() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Runtime
}

// localNow converts now into the configured timezone (routing.timezone), or
// returns it unchanged if unset/"Local"/invalid. Shared by every caller that
// needs the operator-configured wall-clock hour/day (RecordTransition,
// RunPredictionCycle, runSchedules) so they can never diverge from each
// other or from the OS-local zone the binary happens to run under.
func (r *Router) localNow(now time.Time) time.Time {
	r.mu.RLock()
	tz := r.timezone
	r.mu.RUnlock()

	if tz != "" && tz != "Local" {
		if l, err := time.LoadLocation(tz); err == nil {
			now = now.In(l)
		}
	}
	return now
}

// safeRun invokes fn, recovering and logging any panic instead of letting it
// escape. Go has no per-goroutine crash isolation - an unrecovered panic in
// ANY goroutine (not just main's) terminates the entire process, and this
// mesh is architecturally a single process for the whole fleet. Start's
// background maintenance tasks all run against persisted/live state that can
// contain surprises (a stale DB row, an unexpected node response); one bad
// input in any of them must degrade that one task for one cycle, never take
// the whole mesh down and lock an operator out of everything they run.
func safeRun(label string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("router: recovered panic in %s: %v", label, rec)
		}
	}()
	fn()
}

func (r *Router) Start(ctx context.Context) {
	safeRun("pollNvidiaAll", r.pollNvidiaAll)
	safeRun("pollAll", r.pollAll)
	safeRun("discoverAndAddDockerNodes", r.discoverAndAddDockerNodes)
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
	go safeRun("pingWarmupModels", func() { r.pingWarmupModels(ctx) }) // initial ping on startup
	warmupTicker := time.NewTicker(warmupInterval)
	defer warmupTicker.Stop()
	warmupTickerC := warmupTicker.C

	// Schedule ticker: evaluates time-of-day warmup/drain/undrain schedules once
	// a minute (runSchedules dedupes so it fires each schedule at most once per
	// matching minute).
	scheduleTicker := time.NewTicker(1 * time.Minute)
	defer scheduleTicker.Stop()

	// Warm-state flush ticker: Tier 2 of persistence - snapshots the full residency
	// map to SQLite every 60s so drift the immediate lifecycle writes miss (VRAM,
	// last-used) is captured even without a load/unload event.
	warmStateTicker := time.NewTicker(warmStateFlushInterval)
	defer warmStateTicker.Stop()

	predictiveTicker := time.NewTicker(5 * time.Minute)
	defer predictiveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Run asynchronously, but skip this tick if the previous pollAll
			// (bounded by each pollNode's 5s timeout) hasn't finished yet -
			// otherwise a hung node would let poll cycles stack goroutines.
			if r.pollInFlight.CompareAndSwap(false, true) {
				go func() {
					defer r.pollInFlight.Store(false)
					safeRun("pollAll", r.pollAll)
				}()
			}
		case <-dockerTicker.C:
			safeRun("discoverAndAddDockerNodes", r.discoverAndAddDockerNodes)
		case <-nvidiaTicker.C:
			safeRun("pollNvidiaAll", r.pollNvidiaAll)
		case <-sweepTicker.C:
			safeRun("sweepAffinity", r.sweepAffinity)
		case <-warmupTickerC:
			go safeRun("pingWarmupModels", func() { r.pingWarmupModels(ctx) })
		case <-scheduleTicker.C:
			safeRun("runSchedules", func() { r.runSchedules(ctx, time.Now()) })
		case <-warmStateTicker.C:
			go safeRun("FlushWarmState", r.FlushWarmState)
		case <-predictiveTicker.C:
			safeRun("RunPredictionCycle", func() { r.RunPredictionCycle(ctx, time.Now()) })
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
	// Reject a node whose URL already resolves to an existing, differently-named
	// node. config.Validate() only catches duplicate URLs within a single
	// config.yaml; it cannot see nodes that arrived from the DB store overlay,
	// the admin "add node" API, or Docker auto-discovery, all of which funnel
	// through AddNode. Without this check, the SAME physical backend can end up
	// registered twice under two names (e.g. a statically-configured "pve" and
	// an auto-discovered "discovered-ollama-1" both pointing at
	// 192.168.1.115:11434), which silently doubles perceived capacity and splits
	// that node's usage/eviction accounting across two independent NodeStates.
	// Comparison is normalized (case-insensitive scheme/host, trailing-slash
	// agnostic) so cosmetic differences don't defeat the check. First-seen wins;
	// the duplicate is logged loudly and dropped rather than silently added.
	normURL := config.NormalizeNodeURL(n.URL)
	r.mu.RLock()
	for _, existing := range r.nodes {
		if existing.Name == n.Name {
			continue
		}
		if config.NormalizeNodeURL(existing.URL) == normURL {
			r.mu.RUnlock()
			log.Printf("router: WARNING: rejecting node %q (%s): URL already registered as node %q - refusing to register the same backend twice under different names", n.Name, n.URL, existing.Name)
			return
		}
	}
	r.mu.RUnlock()
	node := &NodeState{
		Name:          n.Name,
		URL:           n.URL,
		GPUModel:      n.GPUModel,
		NvidiaIndex:   n.NvidiaIndex,
		VRAMOverrides: n.VRAMOverrides,
		Healthy:       true,
		FirstSeenAt:   time.Now(),
		Runtime:       n.Runtime,
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
	delete(r.prevAgentPresent, name)
	delete(r.nodeAgents, name)
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
				// Same name, different URL - remove old first.
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
// but in-flight connections are allowed to finish. reason records why (e.g.
// "manual", "thermal", "scheduled") and is surfaced to operators in the UI.
// Returns false if not found.
func (r *Router) DrainNode(name string, reason string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == name {
			n.mu.Lock()
			n.Draining = true
			n.DrainedReason = reason
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
			n.DrainedReason = ""
			nodeURL := n.URL
			n.mu.Unlock()
			r.fireWebhook("node_undrain", name, nodeURL)
			return true
		}
	}
	return false
}

// SetPrewarmDisabled toggles whether the predictive engine may warm new
// models onto this node. Live, in-memory only - it is never persisted and
// always reverts to enabled on restart. Returns false if the node is not
// found.
func (r *Router) SetPrewarmDisabled(name string, disabled bool) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == name {
			n.mu.Lock()
			n.PrewarmDisabled = disabled
			n.mu.Unlock()
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
	Runtime     *string `json:"runtime"`
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
			if patch.Runtime != nil {
				n.Runtime = *patch.Runtime
				// "auto" re-arms detection so the next poll re-probes the
				// node instead of keeping whatever runtime it had before.
				n.autoDetect = *patch.Runtime == "auto"
				if !n.autoDetect {
					// Mirror New()/AddNode(): an explicit runtime must always
					// carry a matching probe. Without this, a node still
					// awaiting its first auto-detect (probe == nil) that gets
					// patched straight to an explicit runtime would leave
					// autoDetect false with probe still nil - pollNode's
					// needsDetect guard would then never re-arm detection,
					// and the next poll dereferences a nil probe and panics
					// (crashes the whole single-process mesh, R1/architecture
					// law: one process for the entire mesh).
					n.probe = runtimepkg.NewProbe(*patch.Runtime, r.client)
				}
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

// FindNodeByURL returns the name of an existing node whose URL matches url
// under normalized comparison (case-insensitive scheme/host, trailing-slash
// agnostic), and true if found. excludeName is skipped so callers can check
// "does this URL belong to some OTHER node" (e.g. an admin API update that
// re-submits the same node's own URL is not a collision). Used to reject
// admin "add node" requests that would silently duplicate an already-known
// backend under a second name - see AddNode for the same check applied to
// the DB store overlay and Docker discovery paths.
func (r *Router) FindNodeByURL(url string, excludeName string) (string, bool) {
	norm := config.NormalizeNodeURL(url)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if n.Name == excludeName {
			continue
		}
		if config.NormalizeNodeURL(n.URL) == norm {
			return n.Name, true
		}
	}
	return "", false
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
	n := r.FindNode(nodeName)
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
