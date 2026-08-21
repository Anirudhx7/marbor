package router

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/marboragent"
	runtimepkg "github.com/ollama-mesh/ollama-mesh/internal/runtime"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// hostOrDefault returns host if non-empty, else rawURL's bare hostname -
// the shared default used by both New (statically-configured nodes) and
// AddNode (store/admin/Docker-discovered nodes) so every NodeState always
// has a real, non-empty Host to key its Node Agent config by, even for a
// node whose config.NodeConfig.Host was never explicitly set.
// ResultingHost predicts the Host UpdateNodeURL would assign to a node
// currently at currentHost/currentURL if its URL changed to newURL, without
// mutating anything - exported so callers outside package router (e.g.
// admin.go's pre-mutation TLS sibling-consistency validation) can predict the
// resulting Host before a URL-changing mutation happens, instead of
// independently reimplementing this derivation and risking drift from
// UpdateNodeURL's actual behavior.
//
// Mirrors hostOrDefault's "explicit beats derived" convention, but on the
// derived side it must re-derive from newURL rather than reuse currentHost:
// by the time a live node reaches UpdateNodeURL, currentHost is never empty
// (New/AddNode already default it), so a plain hostOrDefault(currentHost,
// newURL) would always keep the stale current-URL-derived hostname.
// Comparing currentHost against what an undeclared Host would have resolved
// to for currentURL distinguishes "was this ever operator-declared" from
// "was this just derived" without needing a separate field on NodeState.
func ResultingHost(currentHost, currentURL, newURL string) string {
	if currentHost == hostOrDefault("", currentURL) {
		return hostOrDefault("", newURL)
	}
	return currentHost
}

func hostOrDefault(host, rawURL string) string {
	if host != "" {
		return host
	}
	parseURL := rawURL
	if !strings.Contains(parseURL, "://") {
		parseURL = "//" + parseURL
	}
	if u, err := url.Parse(parseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return rawURL
}

type ModelInfo struct {
	Name     string `json:"name"`
	SizeVRAM int64  `json:"sizeVram"`
	// Digest is the runtime-reported content digest/checksum for this loaded model, when the
	// runtime exposes one (currently only Ollama). Empty when unknown - never fabricated.
	Digest string `json:"digest,omitempty"`
}

type NodeState struct {
	Name string
	URL  string
	// Host groups this node with any other node sharing the same physical
	// machine, so a single Node Agent process/enrollment/token covers all of
	// them (see SetMarborAgent/MarborAgentSetting, keyed by Host, not Name).
	// Always non-empty in memory: defaulted to the URL's hostname in AddNode
	// when config.NodeConfig.Host is unset, so it never needs a nil check.
	Host         string
	GPUModel     string
	NvidiaIndex  int
	LoadedModels []ModelInfo
	ActiveConns  int32
	// MaxInFlight is this node's resolved per-node in-flight cap override
	// (P64): 0 means "no override - use Router.maxInFlightPerNode". Set at
	// construction from config.NodeConfig.MaxInFlight and updated live by
	// PatchNode, same lifecycle as VRAMTotalMBConfig. Guarded by mu.
	MaxInFlight int
	// TLSFingerprint is this node's TOFU-pinned SHA-256 fingerprint
	// ("SHA256:...") of its Node Agent's TLS certificate (P24): empty means
	// "no pin - plaintext or not yet TLS-enrolled". Set at construction from
	// config/store NodeOverride and updated live by PatchNode, same
	// lifecycle as MaxInFlight. Guarded by mu. See
	// .local/specs/node-agent-tls.md, especially section 15 for the
	// multi-GPU-per-host (shared Host) caveat this field does not itself
	// resolve - see dialTLSContext in tls_dial.go.
	TLSFingerprint string
	RequestsTotal  int64 // atomic: lifetime requests routed to this node
	ColdStarts     int64 // atomic
	WarmHits       int64 // atomic
	TokensTotal    int64 // atomic
	LatencySumMs   int64 // atomic
	LatencyCount   int64 // atomic
	Healthy        bool
	Draining       bool
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
	VRAMOverrides map[string]int64
	// DeclaredGPUIndices is the operator-declared set of physical GPU indices
	// this specific node/runtime instance actually uses (P75 Gap B/C) - see
	// nodeVRAMCapacity's doc comment in internal/admin/catalog.go for why
	// host-scoped agent telemetry (AgentGPUs below) alone cannot answer this.
	// nil/empty means "nothing declared" - existing host-level sizing applies
	// unchanged. Set via PatchNode/NodePatch.GPUIndices, persisted in
	// store.NodeOverride.GPUIndices.
	DeclaredGPUIndices []int
	autoDetect         bool                    // true if config said runtime: auto; cleared after first detection
	probe              runtimepkg.RuntimeProbe // backend-specific health + runtime warm-model probe
	LastErrorAt        time.Time
	SuccessHistory     []bool

	// Node Agent-derived telemetry (see internal/marboragent, .local/specs/node-agent.md).
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
	// (e.g. "status", "models.pull") - the mesh/UI must gate any
	// agent-dependent feature on this list rather than assuming every agent
	// supports everything, since a fleet naturally has agents on different
	// builds over time. AgentPlatform/AgentArchitecture/AgentGPUVendor/
	// AgentRuntime are self-reported agent metadata (runtime.GOOS/GOARCH,
	// selected GPU backend, locally-detected inference runtime) surfaced for
	// debugging a mixed-version/mixed-vendor/mixed-runtime fleet - all
	// cleared alongside AgentPresent so a disabled/unreachable agent never
	// displays stale metadata as current.
	AgentCapabilities []string
	AgentPlatform     string
	AgentArchitecture string
	AgentGPUVendor    string
	AgentRuntime      string
	// AgentRuntimeID pins this node row to one entry of the shared host's
	// polled marboragent.Telemetry.Runtimes array (see agent_poll.go's
	// pollAgentHost) - stable across a port edit to this node's URL, since
	// it's matched by the agent's own opaque runtime_id, not by port. Empty
	// until the first successful match; in-memory only (not persisted to
	// SQLite - a mesh restart simply re-bootstraps the pin via the port
	// heuristic on the next poll, which is harmless). Never surfaced
	// directly in the admin API/UI - it's routing plumbing, not a
	// fleet-debugging fact like AgentNodeID below.
	AgentRuntimeID string
	// AgentNodeID is the agent's self-persisted node_id (internal/marboragent
	// identity.go) - a stable UUID surviving agent binary upgrades and
	// hostname/IP/DNS changes. Not yet used to re-identify a node across a
	// URL change (NodeState is still keyed by URL/Name); surfaced for
	// fleet-debugging/future use, per .local/specs/node-agent.md's protocol
	// v1 design notes.
	AgentNodeID string
	// AgentGPUCount/AgentGPUs/DriverVersion/CUDAVersion are the multi-GPU
	// array and driver-stack metadata from the agent's gpu resource
	// (marboragent.GPUBlock) - AgentGPUs holds the full per-device snapshot
	// for admin API serialization; the mesh's own routing/placement fields
	// above (VRAMTotalMB etc.) stay the single-value aggregate they always
	// were, unaffected by this addition.
	AgentGPUCount int
	AgentGPUs     []marboragent.GPUInfo
	DriverVersion string
	CUDAVersion   string
	// RAMTotalMB/DiskTotalGB/Hostname/UptimeSeconds/BootTime are the agent's
	// host capacity/identity fields (marboragent.HostTelemetry) - same
	// AgentPresent-gated discipline as every other agent-derived field here.
	RAMTotalMB    int64
	DiskTotalGB   float64
	Hostname      string
	UptimeSeconds int64
	BootTime      int64
	// RuntimeVersion/RuntimeStatus are the agent-detected runtime's own
	// reported version and live reachability (marboragent.RuntimeInfo) -
	// distinct from AgentRuntime (just the runtime name, already above).
	RuntimeVersion string
	RuntimeStatus  string
	// AgentControlDiscovered* is what the agent's most recent ControlDriver
	// probe found (marboragent.ControlDiscovery, P43) - purely informational
	// for the admin API's probe/accept UI. The operator-accepted value
	// lifecycle actions actually read lives in ControlConfig (SetNodeControl/
	// NodeControlSetting below), never here - this is never substituted in
	// as a fallback (node-agent-capabilities.md section 5.6).
	AgentControlDiscoveredDriver     string
	AgentControlDiscoveredIdentifier string
	AgentControlDiscoveredEvidence   []string
	// WarmupErrors holds the last warmup-ping failure per model (model ->
	// error string), so a model that never reaches "resident" is diagnosable
	// instead of silently stuck (previously the error was only visible as an
	// unlabeled Prometheus counter bump - see pingWarmupModels in warmer.go).
	// In-memory only, cleared the moment a ping for that model succeeds;
	// never persisted, same lifecycle as PrewarmDisabled.
	WarmupErrors map[string]string
	// UnloadErrors mirrors WarmupErrors for the scheduled-unload path (model
	// -> error string): UnloadModels previously only logged a failed
	// scheduled/agent unload to the mesh process's own stdout, so a schedule
	// could report LastStatus "ok" (dispatch succeeded) while every model's
	// actual unload silently failed with no dashboard-reachable signal.
	// In-memory only, cleared the moment a later unload of that model
	// succeeds; never persisted, same lifecycle as WarmupErrors.
	UnloadErrors map[string]string
	// agentProtocolWarned latches once a poll observes an agent reporting a
	// protocol_version newer than this mesh binary's own
	// marboragent.ProtocolVersion - logged once per node (not every poll
	// cycle) purely for operator visibility during a rolling upgrade where
	// an agent got updated ahead of the mesh. Decoding itself never depends
	// on this - see agent_poll.go.
	agentProtocolWarned bool
	// AgentFailures counts consecutive failed agent polls, mirroring
	// Failures/healthFailureThreshold's hysteresis for the node's own
	// inference-runtime health - a single dropped TCP connection or timeout
	// polling the Node Agent must not immediately blank out its telemetry
	// (fan/RAM/disk/GPU/runtime status) the way clearAgentTelemetry used to
	// on the very first failure. Reset to 0 on the next successful poll.
	AgentFailures int
	// AgentTLSMismatch is true when the most recent agent poll failed
	// specifically because the presented certificate didn't match this
	// host's pinned fingerprint (P24, see tls_dial.go's
	// ErrTLSFingerprintMismatch and agent_poll.go's pollAgentHost) - a
	// distinct condition from a generic network/timeout failure
	// (AgentFailures alone), surfaced to the dashboard as its own status so
	// an operator doesn't mistake a stale pin for ordinary unreachability.
	// Cleared to false on the next successful poll or any poll failure that
	// isn't itself a fingerprint mismatch.
	AgentTLSMismatch bool

	mu sync.RWMutex
}

// TagsCache holds a cached result from /api/tags for a single node.
type TagsCache struct {
	Models    []TagModel
	FetchedAt time.Time
}

// TagModel represents one model entry from /api/tags.
type TagModel struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"` // bytes on disk
	Details struct {
		// Family is Ollama's own architecture classification (e.g. "llama",
		// "gemma3", "bert", "nomic-bert") - used downstream to distinguish
		// chat-capable models from embedding/encoder-only ones (which have no
		// chat-completion endpoint) without fabricating that distinction (R1:
		// this is Ollama's own reported field, not a guess).
		Family string `json:"family"`
	} `json:"details"`
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
	nodes    []*NodeState
	strategy string
	fallback string
	interval time.Duration
	client   *http.Client
	// tlsTransport is the single shared http.Transport backing client and
	// every HTTPClientForNode(...) client (P24) - its DialTLSContext
	// (tls_dial.go) is the one place TLS fingerprint pinning is enforced,
	// for both the poll path and every admin/eviction action-path call site.
	// Built once in New(), never mutated after - safe to read without a
	// lock. See .local/specs/node-agent-tls.md section 6.
	tlsTransport   *http.Transport
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
	tagsInflight map[string]*tagsInflightEntry
	// showCache caches FetchModelShow results per "nodeURL|tag" for 30
	// seconds, same TTL and rationale as tagsCache - Model Advisor's context
	// slider re-queries handleModelRepo on every tick, so without this a
	// single browsing session repeats identical /api/show round-trips to the
	// same node for architecture facts that never change between requests.
	showCache       map[string]modelShowCacheEntry
	showMu          sync.Mutex
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
	// localDegradationChains maps a model to an ordered list of local
	// alternates to try when no node can serve it at all, gated by a
	// per-request opt-in header (see config.RoutingConfig.LocalDegradationChains).
	// Opt-in, immutable after construction (config-only, not runtime-toggleable).
	localDegradationChains map[string][]string
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
	// modelDigests remembers the first-observed content digest per model
	// name across all nodes (see ModelInfo.Digest - currently Ollama-only).
	// Used to detect a name collision between two different sets of weights
	// (a stale re-pull, a mismatched quantization) so warm-residency scoring
	// doesn't silently treat them as fungible. See recordModelDigest/
	// digestMismatch in placement.go and .local/audit-fixes-2026-08-03.md #4.
	modelDigests map[string]string
	digestMu     sync.RWMutex
	// notifyCh is closed and recreated to broadcast wakes when a connection is freed.
	notifyCh      chan struct{}
	notifyMu      sync.Mutex
	queueDepth    int32 // atomic, current waiters in WaitForNode
	queueMaxDepth int
	queueTimeout  time.Duration
	// maxInFlightPerNode is the global default in-flight cap (P64) - 0 means
	// uncapped. Overridable per node via NodeState.MaxInFlight. Set once at
	// construction (same as queueMaxDepth/overflowSLA); a live change requires
	// a restart, consistent with the other RoutingConfig numeric knobs above.
	maxInFlightPerNode int
	timezone           string // timezone location name (e.g. "UTC", "Asia/Kolkata", "Local")
	warmupCfg          config.WarmupConfig
	// nodeWarmup holds per-node runtime warmup settings toggled via the admin API
	// and persisted in the KV store. Merged with warmupCfg by the warm loop.
	// Guarded by r.mu.
	nodeWarmup map[string]NodeWarmup
	// marborAgents holds per-HOST Node Agent poll configuration (enabled,
	// port, bearer token), keyed by NodeState.Host - not by node name - so
	// every node sharing a physical machine polls the same agent process
	// with the same token (see pollAgentHost in agent_poll.go). Toggled via
	// the admin API (resolved from a node name to its Host first) and
	// persisted in the marbor_agent table (internal/store) under that same
	// host-string key. Guarded by r.mu, same pattern as nodeWarmup. A host
	// absent from this map (or present with Enabled: false) means every
	// node on it is polled for /api/ps as normal but never has its agent
	// fields (AgentPresent, FanPercent, RAMUsedMB, DiskFreeGB) populated.
	marborAgents map[string]MarborAgentConfig
	// nodeControl holds the per-node accepted ControlDriver config (P43),
	// guarded by r.mu same as marborAgents. Absent (or Configured: false)
	// means lifecycle actions must return the "no control driver
	// configured" error rather than guessing.
	nodeControl map[string]ControlConfig
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
	// lastKnownVRAM caches the most recent REAL /api/ps size_vram observed for a
	// (node, model) pair, keyed by modelKey, surviving after the model is
	// unloaded/evicted. estimateModelSizeBytes prefers this over the on-disk
	// weights size, since a model that doesn't fully fit in VRAM (partial
	// GPU+CPU split) can have a real VRAM footprint far smaller than its file
	// size - using the file size there overstates every future headroom
	// reservation for it without bound. Guarded by vramSeenMu.
	vramSeenMu    sync.Mutex
	lastKnownVRAM map[string]int64
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
	// warmupSuppressed marks (node, model) pairs that a manual or scheduled
	// unload just took cold, so pingWarmupModels skips them until explicitly
	// re-armed (SetNodeWarmup or a "warmup" schedule/WarmModels call) instead of
	// silently reloading them on the next warmup tick. Guarded by suppressMu.
	suppressMu       sync.Mutex
	warmupSuppressed map[string]map[string]suppressedInfo
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
	// rr is assigned the real *Router immediately after it's constructed
	// below (once, at the end of this function) - the transport is built
	// first (client is needed while constructing per-node runtime probes,
	// further down) so DialTLSContext closes over rr by reference instead
	// of requiring the Router to already exist. By the time any real dial
	// happens, New() has returned and rr is set. See tls_dial.go and
	// .local/specs/node-agent-tls.md section 6.
	var rr *Router
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return rr.dialTLSContext(ctx, network, addr)
		},
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	nodes := make([]*NodeState, len(nodesCfg))
	for i, n := range nodesCfg {
		ns := &NodeState{
			Name:              n.Name,
			URL:               n.URL,
			Host:              hostOrDefault(n.Host, n.URL),
			GPUModel:          n.GPUModel,
			NvidiaIndex:       n.NvidiaIndex,
			VRAMTotalMBConfig: n.VRAMTotalMB,
			VRAMOverrides:     n.VRAMOverrides,
			MaxInFlight:       n.MaxInFlight,
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
	r := &Router{
		nodes:                    nodes,
		strategy:                 cfg.Strategy,
		fallback:                 cfg.Fallback,
		interval:                 time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		client:                   client,
		tlsTransport:             transport,
		rules:                    cfg.Rules,
		clouds:                   cloudsCopy,
		discoveredURLs:           make(map[string]struct{}),
		prevHealthy:              prev,
		prevAgentPresent:         make(map[string]bool),
		tagsCache:                make(map[string]*TagsCache),
		tagsInflight:             make(map[string]*tagsInflightEntry),
		showCache:                make(map[string]modelShowCacheEntry),
		upstreamTimeout:          upstreamTimeout,
		maxRetries:               maxRetries,
		healthFailureThreshold:   healthFailureThreshold,
		healthSuccessThreshold:   healthSuccessThreshold,
		fallbackChains:           cfg.FallbackChains,
		localDegradationChains:   cfg.LocalDegradationChains,
		overflowSLA:              time.Duration(cfg.OverflowSLAMs) * time.Millisecond,
		thermalWatchdog:          thermalWatchdog,
		affinity:                 make(map[string]*affinityEntry),
		affinityTTL:              affinityTTL,
		sessionAffinity:          cfg.SessionAffinity,
		modelDigests:             make(map[string]string),
		nodeWarmup:               make(map[string]NodeWarmup),
		marborAgents:             make(map[string]MarborAgentConfig),
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
		maxInFlightPerNode:       cfg.MaxInFlightPerNode,
		lastAccuracyLogAt:        time.Now(),
		lastTimeOfDayPrewarmHour: -1,
	}
	rr = r
	return r
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
	// Config change re-arms warmup: an operator turning keep-warm back on (or
	// changing which models it covers) always wins over a prior unload's
	// suppression, else a still-suppressed model would never warm again.
	r.clearAllWarmupSuppress(name)
}

// NodeWarmupSetting returns a copy of the per-node warmup config for name (the
// zero value if unset).
func (r *Router) NodeWarmupSetting(name string) NodeWarmup {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nw := r.nodeWarmup[name]
	return NodeWarmup{Enabled: nw.Enabled, Models: append([]string(nil), nw.Models...)}
}

// MarborAgentConfig is the router's in-memory view of a host's Node Agent
// poll configuration: whether the agent is enabled, which port it listens
// on, and the bearer token the mesh presents when polling it. One config
// per physical host, shared by every node row on that host.
type MarborAgentConfig struct {
	Enabled bool
	Port    int
	Token   string `json:"-"`
	// Scheme is the agent's own transport scheme ("http" or "https"),
	// independent of the node's runtime URL scheme - see
	// store.MarborAgentRecord.Scheme's doc comment. Always "http" or "https";
	// SetMarborAgent defaults it to "http" if passed empty.
	Scheme string
}

// SetMarborAgent sets the per-HOST Node Agent poll config (admin-toggled,
// store-persisted by the caller under the same host key). Disabling removes
// the host from the map entirely so pollAgentHost's "no agent configured"
// branch runs on the next poll, clearing any previously-reported agent
// fields for every node on that host.
func (r *Router) SetMarborAgent(host string, enabled bool, port int, token string, scheme string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.marborAgents == nil {
		r.marborAgents = make(map[string]MarborAgentConfig)
	}
	if !enabled {
		delete(r.marborAgents, host)
		return
	}
	if scheme == "" {
		scheme = "http"
	}
	r.marborAgents[host] = MarborAgentConfig{Enabled: true, Port: port, Token: token, Scheme: scheme}
}

// MarborAgentSetting returns the agent config for the HOST that name's node
// belongs to, and whether one is configured (enabled) at all. Callers that
// already have a host string (e.g. pollAgentHost) should index marborAgents
// directly instead - this is the node-name-based convenience for admin.go
// handlers that still receive a node name from the URL path.
func (r *Router) MarborAgentSetting(name string) (MarborAgentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	host, ok := r.hostOfLocked(name)
	if !ok {
		return MarborAgentConfig{}, false
	}
	cfg, ok := r.marborAgents[host]
	return cfg, ok
}

// MarborAgentSettingByHost returns the agent config for a bare host string
// directly (no node-name resolution) - for callers like validateTLSPatch
// that must check the config for a node's RESULTING host (post-patch),
// which a name-based lookup cannot express since name still resolves to the
// node's CURRENT (pre-mutation) host.
func (r *Router) MarborAgentSettingByHost(host string) (MarborAgentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.marborAgents[host]
	return cfg, ok
}

// NodeHost returns the Host that name's node belongs to, and whether name
// was found at all. admin.go handlers use this to resolve a node name (from
// the URL path) to the shared host key before reading/writing marbor_agent -
// see SetMarborAgent/MarborAgentSetting's doc comments.
func (r *Router) NodeHost(name string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hostOfLocked(name)
}

// hostOfLocked returns the Host of the node named name - caller must already
// hold r.mu (read or write lock).
func (r *Router) hostOfLocked(name string) (string, bool) {
	for _, n := range r.nodes {
		if n.Name == name {
			n.RLock()
			host := n.Host
			n.RUnlock()
			return host, true
		}
	}
	return "", false
}

// ControlConfig is the router's in-memory view of a node's accepted
// ControlDriver configuration (P43) - Driver/Identifier are the operator-
// accepted values lifecycle actions read; Configured is false until an
// operator explicitly accepts one (node-agent-capabilities.md section 5.6).
type ControlConfig struct {
	Driver     string
	Identifier string
	Configured bool
	// StartCommand is the Process driver's launch command (Step 3) - only
	// meaningful when Driver=="process", carried alongside Driver/Identifier
	// so a lifecycle dispatch never has to re-read the store mid-request.
	StartCommand string
}

// SetNodeControl sets the per-node ControlDriver config (admin-toggled,
// store-persisted by the caller). Removing the node from the map entirely
// on !configured mirrors SetMarborAgent's disable behavior.
func (r *Router) SetNodeControl(name string, cfg ControlConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodeControl == nil {
		r.nodeControl = make(map[string]ControlConfig)
	}
	if !cfg.Configured {
		delete(r.nodeControl, name)
		return
	}
	r.nodeControl[name] = cfg
}

// NodeControlSetting returns the accepted ControlDriver config for name and
// whether one is configured at all.
func (r *Router) NodeControlSetting(name string) (ControlConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.nodeControl[name]
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
			req.Header.Set("X-Marbor-Signature", fmt.Sprintf("sha256=%s", sig))
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
	safeRun("pollAgentHosts", r.pollAgentHosts)
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
			// Runs alongside pollAll, not nested inside it - one poll per
			// physical host (see pollAgentHosts), independent of the
			// per-node /api/ps health poll's own in-flight guard above.
			go safeRun("pollAgentHosts", r.pollAgentHosts)
		case <-dockerTicker.C:
			safeRun("discoverAndAddDockerNodes", r.discoverAndAddDockerNodes)
		case <-nvidiaTicker.C:
			safeRun("pollNvidiaAll", r.pollNvidiaAll)
		case <-sweepTicker.C:
			safeRun("sweepAffinity", r.sweepAffinity)
			safeRun("FlushAffinity", r.FlushAffinity)
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

// AddNode registers a node with the router, or - if a node with the same
// Name is already live - upserts its config in place. This mirrors
// sqliteStore.UpsertNode's INSERT OR REPLACE-by-name semantics exactly: the
// DB layer has always treated a repeat POST /admin/nodes for an existing
// name as "replace this row's config," never as "add a second row." Before
// this fix, the in-memory router disagreed - it silently appended a SECOND
// live *NodeState for the same name (independently polled, doubling that
// node's perceived capacity/routing weight) while the DB still correctly
// held one row, an inconsistency that self-healed only on the next mesh
// restart (DB rehydration produces one node). Reachable today via a plain
// double-click of "Add Node" in the UI, and would become a routine
// operation once fleet registration is automated (e.g. a re-run Ansible
// playbook). Upsert is done by mutating the EXISTING *NodeState in place
// (never replacing the pointer), so telemetry/health/warm-residency/session-
// affinity tracked against this node's identity survive the upsert for free
// - there is no separate "preserve vs. reset" step to get wrong.
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
	//
	// While walking r.nodes for that check, also look for an existing node
	// with the SAME name - if found, this call is an upsert, not a fresh add.
	normURL := config.NormalizeNodeURL(n.URL)
	r.mu.RLock()
	var existingByName *NodeState
	for _, existing := range r.nodes {
		if existing.Name == n.Name {
			existingByName = existing
			continue
		}
		if config.NormalizeNodeURL(existing.URL) == normURL {
			r.mu.RUnlock()
			log.Printf("router: WARNING: rejecting node %q (%s): URL already registered as node %q - refusing to register the same backend twice under different names", n.Name, n.URL, existing.Name)
			return
		}
	}
	r.mu.RUnlock()

	if existingByName != nil {
		// Upsert-by-name: update config fields on the SAME NodeState rather
		// than appending a new one. Deliberately unconditional on the URL
		// changing too - the DB's INSERT OR REPLACE already replaces the URL
		// on a same-name POST with zero protection, so matching that is the
		// correct minimal fix, not a new restriction.
		existingByName.mu.Lock()
		existingByName.URL = n.URL
		existingByName.Host = hostOrDefault(n.Host, n.URL)
		existingByName.GPUModel = n.GPUModel
		existingByName.NvidiaIndex = n.NvidiaIndex
		existingByName.VRAMOverrides = n.VRAMOverrides
		existingByName.MaxInFlight = n.MaxInFlight
		existingByName.Runtime = n.Runtime
		if n.Runtime == "auto" {
			existingByName.autoDetect = true
			existingByName.probe = nil // re-armed; pollNode probes on next cycle
		} else {
			existingByName.autoDetect = false
			existingByName.probe = runtimepkg.NewProbe(n.Runtime, r.client)
		}
		existingByName.mu.Unlock()
		// Refresh immediately against the (possibly new) URL/runtime, same as
		// a fresh AddNode. pollNode is a single one-shot probe - it is not a
		// persistent per-node loop (recurring polling comes from pollAll on
		// Router.Start's ticker, which re-reads r.nodes every cycle) - so
		// calling it again here cannot leak a goroutine or race a prior
		// invocation for this node past its own single pass.
		go r.pollNode(existingByName)
		return
	}

	node := &NodeState{
		Name:          n.Name,
		URL:           n.URL,
		Host:          hostOrDefault(n.Host, n.URL),
		GPUModel:      n.GPUModel,
		NvidiaIndex:   n.NvidiaIndex,
		VRAMOverrides: n.VRAMOverrides,
		MaxInFlight:   n.MaxInFlight,
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
	var urlToRemove, hostRemoved string
	for i, n := range r.nodes {
		if n.Name == name {
			urlToRemove, hostRemoved = n.URL, n.Host
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			break
		}
	}
	delete(r.prevHealthy, name)
	delete(r.prevAgentPresent, name)
	// marborAgents is keyed by Host, shared by every node on that host - only
	// drop the entry once no other node still references this host, so
	// removing one node on a multi-runtime host doesn't disable the agent
	// for its siblings.
	if hostRemoved != "" {
		stillShared := false
		for _, n := range r.nodes {
			if n.Host == hostRemoved {
				stillShared = true
				break
			}
		}
		if !stillShared {
			delete(r.marborAgents, hostRemoved)
		}
	}
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
	// GPUIndices declares which physical GPU indices this node/runtime
	// instance actually uses (P75 Gap B/C) - nil means "not present in this
	// PATCH, no change"; a non-nil pointer to an empty slice explicitly
	// clears a prior declaration. See NodeState.DeclaredGPUIndices.
	GPUIndices *[]int `json:"gpu_indices"`
	// URL is handled separately from the other fields - see UpdateNodeURL -
	// but is decoded here so a single PATCH body can carry it.
	URL *string `json:"url"`
	// MaxInFlight declares this node's per-node in-flight cap override (P64) -
	// nil means "not present in this PATCH, no change"; a non-nil pointer to
	// 0 explicitly clears any prior override back to "use the global default"
	// (mirrors GPUIndices' non-nil-empty-slice-clears convention).
	MaxInFlight *int `json:"max_in_flight"`
	// TLSFingerprint declares this node's TOFU-pinned Node Agent cert
	// fingerprint (P24) - nil means "not present in this PATCH, no change";
	// a non-nil pointer to "" explicitly clears a prior pin (reset flow,
	// see .local/specs/node-agent-tls.md section 2/5). No-downgrade
	// enforcement (rejecting a patch that would leave this set alongside an
	// http:// URL) and the section 15 sibling-consistency guard both live in
	// admin.go's handlePatchNode, not here - PatchNode itself only merges.
	TLSFingerprint *string `json:"tls_fingerprint"`
}

// UpdateNodeURL rewrites a node's backend address. Unlike PatchNode's other
// fields, URL cannot be mutated on the live *NodeState in place: proxy.go
// reads node.URL on the hot path with no lock at all (it relies on URL never
// changing once a *NodeState exists), so writing it under n.mu while proxy.go
// reads it unlocked would be a data race. Instead this replaces the slice
// entry wholesale under r.mu.Lock() - the same discipline AddNode/RemoveNode
// already use - preserving the metadata that still applies (runtime, VRAM
// overrides, GPU model) but resetting health/warm-state, since the node now
// points at a different physical address and its prior live state is stale.
// Returns an error if the node is not found or the new URL collides with
// another node's.
func (r *Router) UpdateNodeURL(name string, newURL string) error {
	if err := config.ValidateNodeURL(newURL); err != nil {
		return err
	}
	normURL := config.NormalizeNodeURL(newURL)

	r.mu.Lock()
	var old *NodeState
	idx := -1
	for i, n := range r.nodes {
		if n.Name == name {
			old = n
			idx = i
			continue
		}
		if config.NormalizeNodeURL(n.URL) == normURL {
			r.mu.Unlock()
			return fmt.Errorf("url already registered as node %q", n.Name)
		}
	}
	if old == nil {
		r.mu.Unlock()
		return fmt.Errorf("node %q not found", name)
	}

	old.mu.Lock()
	oldURL := old.URL
	oldHost := old.Host
	runtime := old.Runtime
	autoDetect := old.autoDetect
	gpuModel := old.GPUModel
	nvidiaIndex := old.NvidiaIndex
	vramOverrides := old.VRAMOverrides
	vramTotalMBConfig := old.VRAMTotalMBConfig
	declaredGPUIndices := old.DeclaredGPUIndices
	maxInFlight := old.MaxInFlight
	tlsFingerprint := old.TLSFingerprint
	old.mu.Unlock()

	newHost := ResultingHost(oldHost, oldURL, newURL)

	node := &NodeState{
		Name:               name,
		URL:                newURL,
		Host:               newHost,
		GPUModel:           gpuModel,
		NvidiaIndex:        nvidiaIndex,
		VRAMOverrides:      vramOverrides,
		VRAMTotalMBConfig:  vramTotalMBConfig,
		DeclaredGPUIndices: declaredGPUIndices,
		MaxInFlight:        maxInFlight,
		TLSFingerprint:     tlsFingerprint,
		Healthy:            true,
		FirstSeenAt:        time.Now(),
		Runtime:            runtime,
	}
	if autoDetect {
		node.autoDetect = true
	} else {
		node.probe = runtimepkg.NewProbe(runtime, r.client)
	}
	r.nodes[idx] = node

	delete(r.discoveredURLs, oldURL)
	r.tagsMu.Lock()
	delete(r.tagsCache, oldURL)
	r.tagsMu.Unlock()
	st := r.store
	r.mu.Unlock()

	if st != nil {
		if err := st.DeleteWarmStateByNode(name); err != nil {
			log.Printf("warmstate: delete node %s (url change): %v", name, err)
		}
	}
	go r.pollNode(node)
	return nil
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
			if patch.GPUIndices != nil {
				n.DeclaredGPUIndices = append([]int(nil), (*patch.GPUIndices)...)
			}
			if patch.MaxInFlight != nil {
				n.MaxInFlight = *patch.MaxInFlight
			}
			if patch.TLSFingerprint != nil {
				n.TLSFingerprint = *patch.TLSFingerprint
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

// ModelShowInfo carries the subset of Ollama's /api/show model_info block
// that Model Advisor's context-length feasibility advice (P71) needs to run
// a real per-token KV-cache formula instead of a linear size estimate. Every
// field here is read verbatim from Ollama's own GGUF-derived metadata -
// never guessed - which is what makes this the one runtime where a model's
// true max context and architecture facts are Known rather than Estimated.
type ModelShowInfo struct {
	ContextLength   int64 // <arch>.context_length - the model's trained max context
	BlockCount      int64 // <arch>.block_count - number of transformer layers
	HeadCount       int64 // <arch>.attention.head_count - number of attention heads
	HeadCountKV     int64 // <arch>.attention.head_count_kv - number of KV heads (GQA)
	EmbeddingLength int64 // <arch>.embedding_length - hidden size
}

// modelShowCacheEntry holds a cached FetchModelShow result (success or
// failure) for the showCache TTL cache.
type modelShowCacheEntry struct {
	Info      ModelShowInfo
	OK        bool
	FetchedAt time.Time
}

const modelShowCacheTTL = 30 * time.Second

// FetchModelShow calls a node's Ollama /api/show endpoint for one model tag
// and extracts the architecture facts P71 needs. Returns ok=false (never an
// error the caller must handle) whenever the node/model doesn't yield a
// complete set of facts - a formula missing even one input isn't reliable,
// so a partial answer is treated the same as no answer (R1). Cached for 30s
// per (nodeURL, tag) - same TTL as FetchModelTags - since Model Advisor's
// context-length slider re-triggers this on every tick and the underlying
// architecture facts never change between requests for the same model.
func (r *Router) FetchModelShow(nodeURL, tag string) (ModelShowInfo, bool) {
	cacheKey := nodeURL + "|" + tag
	r.showMu.Lock()
	if cached, ok := r.showCache[cacheKey]; ok && time.Since(cached.FetchedAt) < modelShowCacheTTL {
		r.showMu.Unlock()
		return cached.Info, cached.OK
	}
	r.showMu.Unlock()

	info, ok := r.fetchModelShowUncached(nodeURL, tag)

	r.showMu.Lock()
	r.showCache[cacheKey] = modelShowCacheEntry{Info: info, OK: ok, FetchedAt: time.Now()}
	r.showMu.Unlock()

	return info, ok
}

// fetchModelShowUncached does the actual /api/show HTTP round-trip; see
// FetchModelShow for the caching wrapper around it.
func (r *Router) fetchModelShowUncached(nodeURL, tag string) (ModelShowInfo, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]string{"name": tag})
	if err != nil {
		return ModelShowInfo{}, false
	}
	req, err := http.NewRequestWithContext(ctx, "POST", nodeURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return ModelShowInfo{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return ModelShowInfo{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ModelShowInfo{}, false
	}

	var showResp struct {
		ModelInfo map[string]interface{} `json:"model_info"`
	}
	// A node's Ollama instance is a trusted-ish but not infallible peer - cap
	// the decoded body so a misbehaving or compromised node can't stream an
	// unbounded response into memory for the full 5s window (same 10MB
	// ceiling FetchModelTags' /api/tags decode would benefit from, chosen
	// generously above any real /api/show payload size).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&showResp); err != nil {
		return ModelShowInfo{}, false
	}
	arch, _ := showResp.ModelInfo["general.architecture"].(string)
	if arch == "" {
		return ModelShowInfo{}, false
	}
	num := func(key string) (int64, bool) {
		v, ok := showResp.ModelInfo[arch+"."+key]
		if !ok {
			return 0, false
		}
		f, ok := v.(float64)
		if !ok {
			return 0, false
		}
		return int64(f), true
	}

	ctxLen, ok1 := num("context_length")
	blocks, ok2 := num("block_count")
	headCount, ok3 := num("attention.head_count")
	headCountKV, ok4 := num("attention.head_count_kv")
	embed, ok5 := num("embedding_length")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || ctxLen <= 0 || blocks <= 0 || headCount <= 0 || headCountKV <= 0 || embed <= 0 {
		return ModelShowInfo{}, false
	}
	if embed < headCount {
		// embedding_length/attention.head_count (head_dim) would truncate to
		// 0 via integer division downstream, silently zeroing the entire
		// KV-cache term while still being labeled "derived" (high
		// confidence) - malformed model_info is data this codebase can't
		// trust, not data it should guess a zero-cost answer from (R1).
		return ModelShowInfo{}, false
	}
	return ModelShowInfo{
		ContextLength:   ctxLen,
		BlockCount:      blocks,
		HeadCount:       headCount,
		HeadCountKV:     headCountKV,
		EmbeddingLength: embed,
	}, true
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
