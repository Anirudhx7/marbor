// Package marboragent implements the Marbor agent Protocol v1: the
// node-local execution point for the marbor. v1 ships a read-only "status"
// resource (GPU/host/runtime facts reported back to marbor on its existing
// poll cycle) plus the first mutating resource (model pull) - future
// versions add more node-local resources (runtime restart/drain, more model
// lifecycle, diagnostics) behind the same protocol, versioned and
// capability-gated rather than as a parallel surface. The protocol is
// pull-only transport with a versioned JSON protocol and per-node opaque
// bearer tokens; this file implements the resource envelope
// (status/host/GPU/runtime/control/health).
package marboragent

import "time"

// ProtocolVersion is the current Marbor Agent Protocol version served at
// GET /v1/status. New fields added to Telemetry/GPUInfo/HostTelemetry/
// RuntimeInfo must be optional (nil/omitted means "unknown", never
// fabricated) so an older agent talking to a newer marbor, or vice versa,
// never breaks (the same additive-only wire-compatibility discipline
// extended to this protocol). A bump is reserved for a genuinely breaking
// change.
const ProtocolVersion = 1

// capabilities lists what this agent build actually does, so the marbor (and
// its UI) can enable/disable features per node instead of assuming every
// agent supports everything. Naming convention (binding for every future
// addition): "resource.verb" - e.g. "models.pull", "models.delete",
// "runtime.restart" - mirroring the resource namespace 1:1 so a capability
// string always tells you which resource+route it gates. Appended in the
// same commit that actually implements the feature it names - never
// speculatively, since an agent claiming a capability it doesn't have would
// be exactly the kind of fabrication the "never fabricate" discipline exists
// to prevent, just applied to
// self-description instead of a measurement.
// "transport.tls" is unconditional, not gated on whether this
// specific agent is currently running over HTTPS - it describes what this
// binary is CAPABLE of (it has the TLS listener code and can be enrolled),
// not current connection state. An agent can be capable and still be dialed
// over plain http:// if the node hasn't been migrated yet (opt-in,
// node-by-node).
var capabilities = []string{"status", "models.pull", "models.list", "models.delete", "models.unload", "runtime.health_check", "runtime.start", "runtime.stop", "runtime.restart", "runtime.logs", "runtime.disk", "transport.tls"}

// Telemetry is the canonical, versioned JSON payload served at
// GET /v1/status - the Marbor Agent Protocol's root resource. GET /metrics
// (Prometheus text format, left unversioned per Prometheus's own
// scrape-target convention) is generated from this same struct, not a second
// collection path.
type Telemetry struct {
	Agent        Agent          `json:"agent"`
	Capabilities []string       `json:"capabilities"`
	Host         *HostTelemetry `json:"host,omitempty"`
	GPU          *GPUBlock      `json:"gpu,omitempty"`
	// Runtime is the first entry of Runtimes, kept for back-compat with a
	// marbor binary older than this field's introduction (additive only,
	// never removed). New marbor code should read Runtimes instead.
	Runtime *RuntimeInfo `json:"runtime,omitempty"`
	// Runtimes is every inference runtime this host-scoped agent detected
	// this cycle (see runtime_detect.go's DetectAll) - a host can legitimately
	// run more than one (e.g. Ollama on :11434 and vLLM on :8000 on the same
	// box). Omitted/empty means none detected, never guessed.
	Runtimes []RuntimeInfo `json:"runtimes,omitempty"`
	Control  *ControlInfo  `json:"control,omitempty"`
	Health   Health        `json:"health"`
	// Deployments is the auto-discovered deployment report (one entry
	// per runtime instance keyed by port/runtime_id). Additive - old Marbor
	// ignores unknown field, old agent omits it (nil -> unknown, never
	// fabricated). One entry per host is NOT sufficient when a host runs
	// two vLLM on :8000 and :8001 with different TP widths - hence per-port
	// keying and port/ID-based fan-out on the server (agent_poll.go:184/343
	// pattern), not blind Host fan-out.
	Deployments []DeploymentReport `json:"deployments,omitempty"`
	// LastUpdated is when this snapshot was actually collected, set by
	// Scheduler.refresh - NOT the time of the HTTP request that served it,
	// since /v1/status and /metrics serve a cached background snapshot (see
	// scheduler.go) rather than collecting fresh on every request. Lets the
	// marbor (or an operator reading /v1/status directly) tell a
	// live-but-slightly-behind reading apart from one that's stopped
	// updating because the collector loop died.
	LastUpdated time.Time `json:"last_updated"`
}

// Agent carries every fleet-debugging/identity fact about this agent
// process - the "what are you, what can you do" block a control plane asks
// before adapting, regardless of OS/GPU-vendor/runtime.
type Agent struct {
	// NodeID is a stable UUID generated once on first install and persisted
	// locally (identity.go) - identifies the *machine*, not the agent
	// software running on it, so it stays the same across an agent binary
	// upgrade, a hostname/IP/DNS change, or a runtime swap. Not yet used
	// marbor-side to re-identify a node across a URL change (NodeState is
	// still keyed by URL) - that's separate, larger future work; this field
	// exists now so it doesn't have to be bolted on as a breaking addition
	// later.
	NodeID string `json:"node_id,omitempty"`
	// Version is the agent binary's reported build version.
	Version string `json:"version,omitempty"`
	// ProtocolVersion is the Marbor Agent Protocol version this response was
	// produced under (see the package-level ProtocolVersion constant).
	ProtocolVersion int `json:"protocol_version"`
	// Build is reserved for a build identifier (commit hash, build date)
	// beyond the semantic Version - always omitted today since the marbor
	// binary doesn't currently track a separate build string (only
	// main.Version, set via ldflags). Never fabricated.
	Build string `json:"build,omitempty"`
	// Platform/Architecture are runtime.GOOS/runtime.GOARCH - always known,
	// never omitted.
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
}

// GPUInfo describes one physical GPU device on the host. Every field the
// node can't measure is nil/zero rather than fabricated; consumers must
// treat a nil pointer (or a zero VRAM value) as "unknown", never a real
// measurement of zero.
type GPUInfo struct {
	// Index is this device's 0-based position within GPUBlock.Devices -
	// stable for the process lifetime, not a hardware bus id.
	Index  int    `json:"index"`
	Vendor string `json:"vendor,omitempty"`
	// Model is the card's reported product name (e.g. "NVIDIA GeForce RTX
	// 4090"), straight from the vendor tool - omitted, never guessed, when
	// the backend doesn't report one.
	Model        string   `json:"model,omitempty"`
	CorePercent  *float64 `json:"core_percent,omitempty"`
	TemperatureC *float64 `json:"temperature_c,omitempty"`
	FanPercent   *float64 `json:"fan_percent,omitempty"`
	PowerWatts   *float64 `json:"power_watts,omitempty"`
	VRAMUsedMB   int64    `json:"vram_used_mb,omitempty"`
	VRAMTotalMB  int64    `json:"vram_total_mb,omitempty"`
}

// GPUBlock is the Marbor Agent Protocol's "gpu" resource: fleet-level metadata
// (Vendor - which GPUCollector was selected; DriverVersion/CUDAVersion -
// properties of the host's driver stack, not any one card) plus one GPUInfo
// per physical device. One agent process always reports every GPU on the
// host as this single array - never one agent per GPU. Vendor is
// reported whenever a GPU backend is selected on this host, even on a cycle
// where Collect() itself fails (Devices/Count then empty) - "which backend
// is selected" is a static fact about the process, not a live reading that
// can fail.
type GPUBlock struct {
	Count         int       `json:"count"`
	Vendor        string    `json:"vendor,omitempty"`
	DriverVersion string    `json:"driver_version,omitempty"`
	CUDAVersion   string    `json:"cuda_version,omitempty"`
	Devices       []GPUInfo `json:"devices"`
}

// HostTelemetry holds stdlib-only host stats (CPU/RAM/disk) plus host
// identity (hostname, uptime/boot time). Fields that can't be measured on
// the current platform without a new dependency are omitted rather than
// guessed (never fabricated, and this project's zero external dependencies rule).
type HostTelemetry struct {
	CPUPercent    *float64 `json:"cpu_percent,omitempty"`
	RAMUsedMB     int64    `json:"ram_used_mb,omitempty"`
	RAMTotalMB    int64    `json:"ram_total_mb,omitempty"`
	DiskFreeGB    float64  `json:"disk_free_gb,omitempty"`
	DiskTotalGB   float64  `json:"disk_total_gb,omitempty"`
	Hostname      string   `json:"hostname,omitempty"`
	UptimeSeconds int64    `json:"uptime_seconds,omitempty"`
	BootTime      int64    `json:"boot_time,omitempty"`
}

// RuntimeInfo is the Marbor Agent Protocol's "runtime" resource - kept
// deliberately generic (name/version/status/warm_models/queue_depth) so it
// never becomes vLLM- or Ollama-shaped. A runtime-specific detail (e.g.
// vLLM's tensor-parallel degree, a future ROCm/TensorRT version) belongs in
// a future runtime-specific resource, never a field here - this is a binding
// design rule, not a v1-only convenience.
type RuntimeInfo struct {
	// Name is the locally-detected inference runtime ("ollama", "vllm",
	// "tgi", "llamacpp", "mlx"). This entire RuntimeInfo is omitted from
	// Telemetry (nil) when no runtime could be identified - never guessed.
	Name string `json:"name,omitempty"`
	// ID is this runtime's stable identity (see runtime_identity.go),
	// independent of Name/Port - both of those are attributes that can
	// change (a port gets reconfigured) without the runtime becoming a
	// "different" one. Marbor-side, a node row pins itself to this ID once
	// matched and never re-derives identity from Port after that (Port is
	// only ever used as a one-time bootstrap heuristic).
	ID string `json:"id,omitempty"`
	// Port is the port this runtime instance answered on this cycle -
	// current-cycle metadata only, never identity.
	Port int `json:"port,omitempty"`
	// Version is the runtime's own reported version, when a version query
	// exists for it (today: "ollama version" only - see runtime_version.go).
	// Omitted, never guessed, for runtimes with no such primitive.
	Version string `json:"version,omitempty"`
	// Status is "up" when the runtime answered a live probe this cycle,
	// "down" when a runtime was detected but didn't respond. Never
	// fabricated - mirrors the same probe/reachability check
	// internal/runtime already uses to read loaded models.
	Status string `json:"status,omitempty"`
	// WarmModels lists model names the runtime reports as currently loaded
	// (from the same probe as Status), when reachable.
	WarmModels []string `json:"warm_models,omitempty"`
	// QueueDepth is reserved for a future runtime-side queue-depth signal -
	// no runtime probe this agent uses exposes one today, so this is never
	// populated yet (0 -> omitted via omitempty, never fabricated).
	QueueDepth int `json:"queue_depth,omitempty"`
}

// ControlInfo is the Marbor Agent Protocol's "control" resource -
// descriptive telemetry of the node's configured ControlDriver, additive
// and sibling to Runtime/Health. An unconfigured node reports Driver=""
// (omitted), Configured=false, Capabilities=nil (omitted) - never a
// fabricated driver name. Discovered carries what the agent's most recent
// probe found, purely informational for the admin API's probe/accept UI -
// never substituted for Driver/Configured by any lifecycle action.
type ControlInfo struct {
	Driver       string            `json:"driver,omitempty"`
	Configured   bool              `json:"configured"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Discovered   *ControlDiscovery `json:"discovered,omitempty"`
}

// ControlDiscovery is what the agent's most recent ControlDriver probe
// found - evidence strings, never a bare confidence label.
type ControlDiscovery struct {
	Driver     string   `json:"driver,omitempty"`
	Identifier string   `json:"identifier,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
}

// Health is the Marbor Agent Protocol's "health" resource - deliberately
// minimal in v1: one honest boolean, never a fabricated aggregate score.
// Reserved to grow into per-dimension checks (gpu/disk/memory/network) or an
// overall+checks[] shape later, without a path/shape break.
type Health struct {
	RuntimeReachable bool `json:"runtime_reachable"`
}
