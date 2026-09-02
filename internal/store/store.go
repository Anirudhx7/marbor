package store

import (
	"errors"
	"time"
)

// ErrNoAdminCreds is returned by GetAdminCreds when no credentials have been stored yet.
var ErrNoAdminCreds = errors.New("store: no admin credentials set")

// ErrNotFound is returned by GetSetting when the key does not exist.
var ErrNotFound = errors.New("store: key not found")

// ErrUserNotFound is returned by GetUserByUsername / GetUserByID when no row matches.
var ErrUserNotFound = errors.New("store: user not found")

// AdminCreds holds the dashboard administrator's login credentials.
type AdminCreds struct {
	Username     string
	PasswordHash string // hex(sha256 iterated over salt+":"+password)
	Salt         string // 32 random bytes as hex
}

// Store is the persistence interface. NopStore{} is the no-op implementation
// (all ops are no-ops) so callers never need to nil-check - a nil Store
// interface value is NOT valid and panics on any method call, same as any
// nil interface in Go; construct NopStore{} explicitly instead.
type Store interface {
	// Request log
	AppendRequest(r RequestRecord) error
	LastRequests(n int) ([]RequestRecord, error)
	// GetRequest looks up one request_log row by id (P41 explain endpoint
	// fallback beyond the bounded in-memory ring). ok is false if no row
	// with that id exists (already trimmed, or never persisted).
	GetRequest(id string) (rec RequestRecord, ok bool, err error)

	// Analytics. UpsertHourlyBucket/UpsertModelStat ACCUMULATE the passed
	// counts into the existing row (callers pass a per-request delta), so
	// totals survive a restart intact.
	UpsertHourlyBucket(b HourlyBucket) error
	HourlyBuckets(since time.Time) ([]HourlyBucket, error)
	UpsertModelStat(s ModelStat) error
	AllModelStats() ([]ModelStat, error)

	// Global counters
	SetCounters(c Counters) error
	GetCounters() (Counters, error)

	// Per-key usage (replaces auth/persist.go JSON)
	SaveKeyCounters(name string, snap KeyCounterSnapshot) error
	AllKeyCounters() (map[string]KeyCounterSnapshot, error)

	// Runtime nodes (survive restart)
	UpsertNode(nc NodeRecord) error
	DeleteNode(name string) error
	AllNodes() ([]NodeRecord, error)
	UpdateNodeURL(name string, url string) error

	// Node overrides (vram, gpu_model, declared gpu_indices - P75 Gap B/C;
	// max_in_flight - P64 per-node in-flight cap override; tls_fingerprint -
	// P24 TOFU-pinned Marbor agent cert fingerprint; parallelism_type/width -
	// P397 deployment-aware placement tp|pp|ep|dp)
	UpsertNodeOverride(name string, vramTotalMB *int64, gpuModel *string, runtime *string, gpuIndices *[]int, maxInFlight *int, tlsFingerprint *string, parallelismType *string, parallelismWidth *int) error
	NodeOverrides() (map[string]NodeOverride, error)

	// Node drain state
	SetNodeDrain(name string, draining bool, reason string) error
	NodeDrainStates() (map[string]NodeDrainState, error)

	// Marbor Agent (per-node opaque bearer token + enable/port, encrypted at
	// rest - see internal/marboragent and .local/specs/node-agent.md section 5).
	// GetMarborAgent's error may be returned as-is by callers (single-node
	// lookup, blast radius is that one node's telemetry falling back to "-");
	// AllMarborAgents must never fail the whole list on one bad row (drop and
	// continue, matching AllKeys/AllCloudProviders).
	UpsertMarborAgent(rec MarborAgentRecord) error
	GetMarborAgent(name string) (MarborAgentRecord, bool, error)
	AllMarborAgents() ([]MarborAgentRecord, error)
	DeleteMarborAgent(name string) error

	// Node ControlDriver config (per-node, how the agent controls the
	// runtime process - P43, .local/specs/node-agent-capabilities.md
	// section 5.5). Discovered is freely overwritten by a re-scan;
	// Configured changes only via UpsertNodeControlConfigured, the operator
	// Accept action - never as a side effect of a discovery re-run.
	UpsertNodeControlDiscovered(name, driver, identifier string, evidence []string) error
	// startCommand is the Process driver's launch command (Step 3) - only
	// meaningful when driver=="process", but accepted unconditionally since
	// the store has no opinion on which driver a given node uses.
	UpsertNodeControlConfigured(name, driver, identifier, startCommand string) error
	// ClearNodeControlConfigured un-configures a node (an operator removing
	// its accepted driver) without discarding the discovered/evidence
	// columns, unlike DeleteNodeControl which drops the whole row.
	ClearNodeControlConfigured(name string) error
	GetNodeControl(name string) (NodeControlRecord, bool, error)
	AllNodeControl() ([]NodeControlRecord, error)
	DeleteNodeControl(name string) error

	// Predictive engine transition history (survives restart)
	AppendPredictiveTransition(fromModel, toModel string, ts time.Time) error
	PredictiveHistory() ([]PredictiveTransition, error)

	// Runtime API keys (survive restart)
	UpsertKey(k KeyRecord) error
	RevokeKey(name string) error
	// DeleteKey hard-deletes a key row, unlike RevokeKey (which only flips
	// revoked=1 and keeps the row forever). Used for keys with no long-term
	// meaning to retain - e.g. the in-dashboard hardware benchmark's
	// ephemeral per-run key, which would otherwise leave a permanent
	// "benchmark-<timestamp>" row cluttering the real API Keys page on
	// every single run.
	DeleteKey(name string) error
	AllKeys() ([]KeyRecord, error)
	// KeySpendSince sums real cloud-fallback cost_usd for keyName since the
	// given time, for per-key cloud spend cap checks.
	KeySpendSince(keyName string, since time.Time) (float64, error)
	// IncrSpillCounter increments the (keyName, servedBy) row in
	// spill_counters by one, creating it if absent. servedBy is "local", a
	// cloud provider's Name, or "blocked". Best-effort accounting only - any
	// error here must never affect the caller's inference request; callers
	// should log and ignore it, not propagate it into the request path.
	IncrSpillCounter(keyName, servedBy string) error
	// SpillCounters returns every (key_name, served_by) row fleet-wide. The
	// table is bounded by keys x providers, so a per-key drill-down is just
	// a client-side filter of this same payload - no separate query needed.
	SpillCounters() ([]SpillCounterRow, error)

	// Audit log (replaces audit/audit.go file-based logger)
	AppendAuditLog(e AuditEntry) error
	QueryAuditLog(opts AuditQuery) ([]AuditEntry, error)
	PruneAuditLog(retentionDays int) error
	PruneSystemAuditLog(retentionDays int) error

	// System audit log (administrative mutations)
	AppendSystemAuditLog(e SystemAuditEntry) error
	QuerySystemAuditLog(limit int) ([]SystemAuditEntry, error)
	QuerySystemAuditLogFiltered(f SystemAuditFilter) ([]SystemAuditEntry, error)

	// Admin dashboard credentials
	GetAdminCreds() (AdminCreds, error)
	SetAdminCreds(creds AdminCreds) error

	// Admin dashboard sessions
	CreateSession(token string, expiresAt time.Time) error
	ValidateSession(token string) (bool, error)
	DeleteSession(token string) error
	PruneExpiredSessions() error

	// Multi-user management
	CreateUser(u User) (int64, error)
	GetUserByUsername(username string) (User, error)
	GetUserByID(id int64) (User, error)
	ListUsers() ([]User, error)
	UpdateUser(u User) error
	DeleteUser(id int64) error
	SoftDeleteUser(id int64, deletedBy string) error
	CountAdminUsers() (int, error)
	PendingUserCount() (int, error)

	// User sessions (new auth system)
	CreateUserSession(s UserSession) error
	GetUserSession(token string) (UserSession, bool, error)
	DeleteUserSession(token string) error
	DeleteUserSessionsByUserID(userID int64) error
	PruneExpiredUserSessions() error

	// Migration helpers
	HasAdminCredentials() (bool, error)
	GetLegacyAdminCreds() (username, passwordHash, salt string, err error)

	// Routing rules (persist across restarts - fixes audit finding #30)
	UpsertRoutingRule(r RoutingRuleRecord) error
	DeleteRoutingRule(id string) error
	SetRoutingRuleEnabled(id string, enabled bool) error
	AllRoutingRules() ([]RoutingRuleRecord, error)

	// Settings key-value store (replaces config.SaveConfig)
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
	AllSettings() (map[string]string, error)

	// Cloud providers (replaces config.CloudProviders slice)
	UpsertCloudProvider(cp CloudProviderRecord) error
	DeleteCloudProvider(name string) error
	AllCloudProviders() ([]CloudProviderRecord, error)
	// SetCloudProviderPriorities renumbers providers to match the given order,
	// highest priority first (order[0] gets the highest int).
	SetCloudProviderPriorities(order []string) error

	// Favorites (per-user starred models on the Model Advisor page)
	AddFavorite(userID int64, modelID string) error
	RemoveFavorite(userID int64, modelID string) error
	ListFavorites(userID int64) ([]string, error)

	// Warmup configuration
	GetWarmupConfig() (WarmupConfigRecord, error)
	SetWarmupConfig(enabled bool, keepAlive string) error
	UpsertWarmupModel(model string, nodes []string) error
	DeleteWarmupModel(model string) error
	AllWarmupModels() ([]WarmupModelRecord, error)

	// Warm state - the model residency map (which model is resident on which
	// node), its LRU last-used time, VRAM footprint, and load count. Persisted so
	// the router starts warm after a restart instead of cold (Phase 1). RecordWarmLoad
	// bumps load_count (a model was loaded); SnapshotWarmState refreshes the
	// residency snapshot without touching load_count (the background flush).
	RecordWarmLoad(w WarmStateRecord) error
	SnapshotWarmState(w WarmStateRecord) error
	DeleteWarmState(model, node string) error
	DeleteWarmStateByNode(node string) error
	// ReconcileNodeWarmState makes the persisted residency for a node match the
	// live truth: it deletes every row for the node whose model is NOT in
	// residentModels. An empty residentModels clears the node entirely. This is how
	// stale rows (left by a prior run) are pruned once a live poll reveals the truth.
	ReconcileNodeWarmState(node string, residentModels []string) error
	AllWarmState() ([]WarmStateRecord, error)

	// Session affinity - persisted sticky-session -> node pinning (see
	// AffinityRecord). SnapshotAffinity replaces the whole table with exactly
	// the given entries (mirrors the in-memory map's own bounded, TTL-swept
	// lifecycle - a snapshot-replace, not an ever-growing upsert log).
	SnapshotAffinity(entries []AffinityRecord) error
	AllAffinity() ([]AffinityRecord, error)

	// Model configuration overrides - an operator-declared default parameter
	// profile (load-time engine params, inference-time sampling defaults, meta
	// fields) for a model on a specific node, applied whenever Marbor
	// routes to it. Keyed by (model, node) rather than model alone: the same
	// model name can be resident on nodes with different runtimes (Ollama,
	// vLLM, TGI, llama.cpp) or simply different VRAM budgets, and a single
	// shared profile can't express per-node differences in either case.
	// GetModelConfig returns ErrNotFound if there's no configured profile for
	// that exact (model, node) pair.
	GetModelConfig(model, node string) (ModelConfig, error)
	SetModelConfig(cfg ModelConfig) error
	DeleteModelConfig(model, node string) error
	AllModelConfigs() ([]ModelConfig, error)

	// InsertBenchmarkRun persists one completed in-dashboard hardware
	// benchmark run (see internal/admin's benchmarkJob) for the history
	// table. ListBenchmarkRuns returns the most recent limit runs, newest
	// first.
	InsertBenchmarkRun(run BenchmarkRun) error
	ListBenchmarkRuns(limit int) ([]BenchmarkRun, error)

	// BackupTo writes a consistent point-in-time copy of the live database to
	// path via SQLite's VACUUM INTO, safe to run alongside concurrent readers
	// and writers. path must not already exist (VACUUM INTO refuses to
	// overwrite a file) - callers pass a fresh path per call.
	BackupTo(path string) error

	Close() error
}

// RequestRecord mirrors a single request log entry.
type RequestRecord struct {
	ID         string  `json:"id"`
	KeyName    string  `json:"key_name"`
	Model      string  `json:"model"`
	NodeName   string  `json:"node_name"`
	StatusCode int     `json:"status_code"`
	LatencyMs  int64   `json:"latency_ms"`
	TokensUsed int64   `json:"tokens_used"`
	CostUSD    float64 `json:"cost_usd"`
	RoutedTo   string  `json:"routed_to"`
	IsCloud    bool    `json:"is_cloud"`
	// RoutingReason/RoutingDetail are P41 explainability fields - empty for
	// rows predating this feature or for cloud-fallback requests, which have
	// no router.RoutingDecision. RoutingDetail is the JSON-encoded score
	// breakdown, not a human string.
	RoutingReason string    `json:"routing_reason,omitempty"`
	RoutingDetail string    `json:"routing_detail,omitempty"`
	TS            time.Time `json:"ts"`
}

// HourlyBucket tracks request counts and costs for one UTC hour.
// Hour is truncated to hour boundary (minutes/seconds/ns = 0).
type HourlyBucket struct {
	Hour          time.Time `json:"hour"`
	Requests      int       `json:"requests"`
	Tokens        int64     `json:"tokens"`
	CloudRequests int       `json:"cloud_requests"`
	LocalRequests int       `json:"local_requests"`
	CostUSD       float64   `json:"cost_usd"`
	// GenDurationMs is Ollama's real eval_duration summed across requests in
	// this hour, in milliseconds. 0 for hours with no local Ollama-native
	// responses (cloud requests never report it). tokens/GenDurationMs gives
	// a real average tokens-per-second for the hour.
	GenDurationMs int64 `json:"gen_duration_ms"`
}

// ModelStat tracks aggregate stats per model.
type ModelStat struct {
	Model    string  `json:"model"`
	Requests int     `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

// Counters holds global lifetime totals.
type Counters struct {
	LocalRequests int64   `json:"local_requests"`
	CloudRequests int64   `json:"cloud_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	CloudSpentUSD float64 `json:"cloud_spent_usd"`
}

// KeyCounterSnapshot is the persisted form of per-key usage counters.
type KeyCounterSnapshot struct {
	Today       int       `json:"today"`
	Month       int       `json:"month"`
	TokensToday int64     `json:"tokens_today"`
	TokensMonth int64     `json:"tokens_month"`
	LastReset   time.Time `json:"last_reset"`
}

// NodeRecord is a runtime-discovered or config-declared node that should
// survive a process restart.
type NodeRecord struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Runtime     string `json:"runtime"`
	VRAMTotalMB *int64 `json:"vram_total_mb,omitempty"`
	// Host groups this node with any other node sharing the same physical
	// machine, so they can share one Marbor Agent enrollment/token instead of
	// each needing its own - empty means "not explicitly grouped," resolved
	// to a default (the URL's hostname) in internal/router.AddNode, never
	// left ambiguous in memory.
	Host string `json:"host,omitempty"`
}

// NodeOverride holds operator-declared overrides for a node.
type NodeOverride struct {
	VRAMTotalMB *int64  `json:"vram_total_mb,omitempty"`
	GPUModel    *string `json:"gpu_model,omitempty"`
	Runtime     *string `json:"runtime,omitempty"`
	// GPUIndices is the operator-declared set of physical GPU indices this
	// specific node/runtime instance actually uses (P75 Gap B/C) - host-scoped
	// agent telemetry reports every physical GPU identically to every node
	// sharing a Host, so a node pinned to one GPU (e.g. CUDA_VISIBLE_DEVICES)
	// needs this to avoid being sized against hardware it cannot reach. nil
	// means "nothing declared" (the default - unchanged host-level sizing);
	// a non-nil empty slice explicitly clears a prior declaration.
	GPUIndices *[]int `json:"gpu_indices,omitempty"`
	// MaxInFlight is the operator-declared per-node in-flight cap override
	// (P64) - nil means "nothing declared" (use RoutingConfig.MaxInFlightPerNode).
	MaxInFlight *int `json:"max_in_flight,omitempty"`
	// TLSFingerprint is the TOFU-pinned SHA-256 fingerprint ("SHA256:...") of
	// this node's agent TLS certificate (P24) - nil means "no pin, plaintext
	// or not yet TLS-enrolled". See .local/specs/node-agent-tls.md.
	TLSFingerprint *string `json:"tls_fingerprint,omitempty"`
	// ParallelismType is the deployment topology type (P397) - "" means
	// unconstrained (existing fleet), otherwise "tp"|"pp"|"ep"|"dp".
	// ParallelismWidth is the width for that type - nil means unconstrained.
	// Validated: for tp, len(gpu_indices) >= width when both declared, else 422.
	ParallelismType  *string `json:"parallelism_type,omitempty"`
	ParallelismWidth *int    `json:"parallelism_width,omitempty"`
}

// MarborAgentRecord is the per-node Marbor Agent configuration: whether the
// agent is enabled for this node, which port it listens on, and the opaque
// bearer token marbor presents when polling it. Token is encrypted at
// rest by the sqliteStore implementation (AES-256-GCM, same primitive as
// secretbox.go) - see .local/specs/node-agent.md section 5 for why this is
// a distinct protocol/table from the client-facing API-key mechanism, not a
// reuse of it.
type MarborAgentRecord struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port"`
	Token   string `json:"-"`
	// Scope is the tier (marboragent.ScopeReadonly/ScopeOperator/ScopeAdmin)
	// embedded in Token's prefix (P54: per-action token scoping). Stored
	// alongside Token purely for observability/API responses - the agent
	// enforces scope by parsing its own configured Token directly
	// (marboragent.TokenScope), not by trusting this column, so a mismatch
	// between the two is a display-staleness issue, never a security one.
	// Defaults to "admin" for rows created before this field existed,
	// matching those rows' actual (unprefixed, full-scope-by-fallback)
	// tokens.
	Scope string `json:"scope"`
	// Scheme is the Marbor Agent's OWN transport scheme ("http" or "https") -
	// independent of the node's runtime URL (NodeState.URL/runtime_nodes.url)
	// scheme. Before this field existed, every Marbor Agent URL builder
	// derived its scheme from the runtime URL instead, so enabling HTTPS for
	// the agent silently switched the runtime endpoint to https:// too and
	// broke runtimes (Ollama, vLLM, etc.) that only serve plain HTTP. Always
	// "http" or "https"; defaults to "http" for rows predating this field.
	Scheme string `json:"scheme"`
}

// NodeControlRecord is the per-node ControlDriver configuration (P43) - how
// the Marbor Agent starts/stops/restarts the inference runtime process on
// this node. Discovered* is what the most recent probe found (evidence, not
// a bare confidence label - marbor-agent-capabilities.md section 5.5);
// Driver/Identifier/Configured is what an operator has explicitly accepted
// and the only value lifecycle actions ever read. Configured is false
// until an operator accepts a value - a node with no ControlDriver
// configured stays fully usable for every non-lifecycle action.
type NodeControlRecord struct {
	Name       string `json:"name"`
	Driver     string `json:"driver,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Configured bool   `json:"configured"`

	DiscoveredDriver     string   `json:"discovered_driver,omitempty"`
	DiscoveredIdentifier string   `json:"discovered_identifier,omitempty"`
	DiscoveredEvidence   []string `json:"discovered_evidence,omitempty"`

	// StartCommand is the Process driver's launch command (Step 3) - the
	// bare PID-file convention alone gives no way to know how to launch the
	// process fresh, so an operator configures it explicitly at Accept time.
	// Only meaningful when Driver=="process"; empty for every other driver.
	StartCommand string `json:"start_command,omitempty"`
}

// PredictiveTransition is one persisted model-to-model transition, used to
// reseed the predictive engine's in-memory history after a restart.
type PredictiveTransition struct {
	FromModel string
	ToModel   string
	Timestamp time.Time
}

// KeyRecord is a runtime API key that should survive a process restart.
type KeyRecord struct {
	Name                  string   `json:"name"`
	Key                   string   `json:"key"`
	RateLimit             int      `json:"rate_limit"`
	DailyLimit            int      `json:"daily_limit"`
	MonthlyLimit          int      `json:"monthly_limit"`
	DailyUsdCap           float64  `json:"daily_usd_cap"`
	MonthlyUsdCap         float64  `json:"monthly_usd_cap"`
	Models                []string `json:"models"`
	Revoked               bool     `json:"revoked"`
	ExpiresAt             string   `json:"expires_at"`
	LocalOnly             bool     `json:"local_only"`
	AllowLocalDegradation bool     `json:"allow_local_degradation"`
}

// SpillCounterRow is one (key_name, served_by) count from the spill_counters
// table - "how many requests this key sent to this destination". served_by
// is "local", a cloud provider's Name, or "blocked" (a local_only policy
// rejection). For any given key, summing requests across "local", every
// enabled cloud provider, and "blocked" equals that key's total completed
// routing decisions - a future code path that forgets to increment any
// bucket is detectable as a mismatch against that invariant.
type SpillCounterRow struct {
	KeyName  string `json:"key_name"`
	ServedBy string `json:"served_by"`
	Requests int64  `json:"requests"`
}

// AuditEntry is one structured audit log record persisted to SQLite.
type AuditEntry struct {
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	KeyName       string    `json:"key_name"`
	Model         string    `json:"model"`
	Node          string    `json:"node"`
	Status        string    `json:"status"`
	LatencyMs     int       `json:"latency_ms"`
	Cloud         bool      `json:"cloud"`
	CloudModel    string    `json:"cloud_model,omitempty"`
	RoutingReason string    `json:"routing_reason,omitempty"`
}

// SystemAuditEntry is one administrative mutation event persisted to SQLite.
type SystemAuditEntry struct {
	ID       int64     `json:"id"`
	Time     time.Time `json:"time"`
	Username string    `json:"username"`
	Action   string    `json:"action"`
	Target   string    `json:"target"`
	Details  string    `json:"details"`
	SourceIP string    `json:"source_ip"`
}

// SystemAuditFilter controls server-side filtering for QuerySystemAuditLogFiltered.
// All fields combine with AND. Zero values mean no filter. Kind is the fleet
// operations bucket derived from action via toActivityKind, predictive returns
// 0 rows from this table.
type SystemAuditFilter struct {
	From     *time.Time
	To       *time.Time
	Before   *time.Time
	BeforeID *int64
	Limit    int
	Kind     string
	Action   string
	Username string
	Target   string
	SourceIP string
}

// AuditQuery controls filtering for QueryAuditLog.
type AuditQuery struct {
	Limit int
	Model string
	Key   string
	Node  string
	// StatusCategory buckets by HTTP status range: "success" (2xx),
	// "client_error" (4xx), or "server_error" (5xx). Empty means no filter.
	StatusCategory string
	Cloud          *bool
	Since          time.Time
	Until          time.Time
}

// User is a dashboard administrator or API-consumer user with role-based access.
type User struct {
	ID                 int64      `json:"id"`
	Username           string     `json:"username"`
	Email              string     `json:"email"`
	PasswordHash       string     `json:"-"`
	Salt               string     `json:"-"`
	Role               string     `json:"role"`   // "admin" | "user"
	Status             string     `json:"status"` // "pending" | "active" | "suspended"
	APIKeyName         string     `json:"api_key_name"`
	MustChangePassword bool       `json:"must_change_password"`
	SkipPasswordCount  int        `json:"skip_password_count"`
	CreatedAt          time.Time  `json:"created_at"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	ApprovedBy         string     `json:"approved_by,omitempty"`
	DeletedAt          *time.Time `json:"deleted_at,omitempty"`
	DeletedBy          string     `json:"deleted_by,omitempty"`
}

// UserSession is an authenticated session tied to a specific User row.
type UserSession struct {
	Token              string `json:"-"`
	UserID             int64
	Role               string
	Username           string
	MustChangePassword bool
	ExpiresAt          time.Time
}

// RoutingRuleRecord is a routing rule that persists across restarts.
type RoutingRuleRecord struct {
	ID        string    `json:"id"`
	Condition string    `json:"condition"`
	Target    string    `json:"target"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// CloudProviderRecord stores cloud fallback provider configuration in SQLite.
type CloudProviderRecord struct {
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	BaseURL         string  `json:"base_url"`
	APIKey          string  `json:"api_key"`
	DefaultModel    string  `json:"default_model"`
	CostPer1KTokens float64 `json:"cost_per_1k_tokens"`
	Enabled         bool    `json:"enabled"`
	Priority        int     `json:"priority"`
}

// WarmupConfigRecord stores global warmup enable/keep-alive settings.
type WarmupConfigRecord struct {
	Enabled   bool   `json:"enabled"`
	KeepAlive string `json:"keep_alive"`
}

// WarmupModelRecord stores a model pinned for proactive warming.
type WarmupModelRecord struct {
	Model string   `json:"model"`
	Nodes []string `json:"nodes"` // empty = all nodes
}

// WarmStateRecord is one (model, node) entry of the warm-state residency map:
// which model is resident on which node, when it was last used (the LRU eviction
// key), its VRAM footprint, and how many times it has been loaded. Persisted so
// the router can restore its warm set on startup instead of starting cold.
type WarmStateRecord struct {
	Model     string    `json:"model"`
	Node      string    `json:"node"`
	LastUsed  time.Time `json:"last_used"`  // zero time = never used since load
	VRAMBytes int64     `json:"vram_bytes"` // 0 = unknown
	LoadCount int64     `json:"load_count"`
}

// AffinityRecord is one persisted sticky-session entry: which node a session
// was last pinned to and when it was last seen. Persisted so a marbor restart
// doesn't drop every in-flight sticky session and force a cold KV-cache
// round-trip on the next request (see .local/audit-fixes-2026-08-03.md #7).
// Still only ever a soft preference at restore time - Route always
// re-validates health/draining before honoring a restored entry, exactly as
// it does for one created during normal operation.
type AffinityRecord struct {
	SessionID string    `json:"session_id"`
	NodeURL   string    `json:"node_url"`
	LastSeen  time.Time `json:"last_seen"`
}

// ModelConfig is the operator-declared default parameter profile for a model  --
// covering Ollama's load-time engine params, inference-time sampling defaults,
// and Marbor's own meta/orchestration fields (system prompt override,
// per-model rate caps). Every field is nilable/nullable: nil (or an absent
// key in the persisted JSON) means "not configured, inherit the backend's own
// default" - this struct must never carry a value the operator didn't
// explicitly set (R1: no fabricated defaults masquerading as configuration).
type ModelConfig struct {
	Model string `json:"model"`
	// Node is the specific node this profile applies to (required - a
	// profile with no node has no meaning, since injection always happens
	// against one already-selected node). The same model name may have a
	// separate ModelConfig row per node it's resident on.
	Node string `json:"node"`

	// Load-time / engine parameters - Ollama only. Injected into every routed
	// request's Ollama "options" object; Ollama reloads the model automatically
	// when a resident instance's options differ from an incoming request's, so
	// no separate evict-then-reload step is needed on the marbor side. This list
	// is verified against Ollama's current api/types.go Options/Runner structs
	// (github.com/ollama/ollama) - flash_attention, offload_kv_cache_to_gpu,
	// rope_frequency_base/scale, use_mlock, and tensor_parallelism were removed
	// from this struct because they are not real per-request parameters on ANY
	// of the four runtimes (they're process-launch CLI flags at best, or never
	// existed at all) - keeping them would have been exactly the kind of
	// control that looks functional but isn't (R1).
	NumCtx          *int  `json:"num_ctx,omitempty"`
	NumGPU          *int  `json:"num_gpu,omitempty"`
	MainGPU         *int  `json:"main_gpu,omitempty"`
	NumBatch        *int  `json:"num_batch,omitempty"`
	NumThread       *int  `json:"num_thread,omitempty"`
	UseMmap         *bool `json:"use_mmap,omitempty"`
	DraftNumPredict *int  `json:"draft_num_predict,omitempty"`
	TTL             *int  `json:"ttl,omitempty"`

	// Inference-time / sampling parameters. Injected into a routed request
	// only when the client's own request does not already specify the field.
	// Which of these actually apply to a given runtime is declared in
	// internal/store/model_config_capabilities.go, verified against each
	// runtime's real current source/API schema, not assumed from an older or
	// unrelated runtime's option set.
	Temperature      *float64           `json:"temperature,omitempty"`
	TopP             *float64           `json:"top_p,omitempty"`
	TopK             *int               `json:"top_k,omitempty"`
	MinP             *float64           `json:"min_p,omitempty"`
	TypicalP         *float64           `json:"typical_p,omitempty"`
	NumKeep          *int               `json:"num_keep,omitempty"`
	MaxTokens        *int               `json:"max_tokens,omitempty"`
	Seed             *int               `json:"seed,omitempty"`
	Stop             []string           `json:"stop,omitempty"`
	RepeatPenalty    *float64           `json:"repeat_penalty,omitempty"`
	RepeatLastN      *int               `json:"repeat_last_n,omitempty"`
	PresencePenalty  *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64           `json:"frequency_penalty,omitempty"`
	Mirostat         *int               `json:"mirostat,omitempty"`
	MirostatTau      *float64           `json:"mirostat_tau,omitempty"`
	MirostatEta      *float64           `json:"mirostat_eta,omitempty"`
	LogitBias        map[string]float64 `json:"logit_bias,omitempty"`
	ResponseFormat   *string            `json:"response_format,omitempty"`

	// llama.cpp-only sampling extras (its /completion and OpenAI-compatible
	// endpoints both accept these - llama.cpp's server README explicitly
	// states "/completion-specific features such as mirostat are also
	// supported" on the OpenAI-compatible path).
	DryMultiplier    *float64 `json:"dry_multiplier,omitempty"`
	DryBase          *float64 `json:"dry_base,omitempty"`
	DryAllowedLength *int     `json:"dry_allowed_length,omitempty"`
	DryPenaltyLastN  *int     `json:"dry_penalty_last_n,omitempty"`
	XtcProbability   *float64 `json:"xtc_probability,omitempty"`
	XtcThreshold     *float64 `json:"xtc_threshold,omitempty"`
	NProbs           *int     `json:"n_probs,omitempty"`
	MinKeep          *int     `json:"min_keep,omitempty"`

	// vLLM-only sampling extras (its OpenAI-compatible ChatCompletionRequest
	// accepts these beyond the strict OpenAI schema). IgnoreEOS is shared
	// wire-for-wire with llama.cpp, which uses the identical "ignore_eos"
	// field name and meaning.
	LengthPenalty          *float64 `json:"length_penalty,omitempty"`
	StopTokenIDs           []int    `json:"stop_token_ids,omitempty"`
	IncludeStopStrInOutput *bool    `json:"include_stop_str_in_output,omitempty"`
	IgnoreEOS              *bool    `json:"ignore_eos,omitempty"`
	MinTokens              *int     `json:"min_tokens,omitempty"`
	SkipSpecialTokens      *bool    `json:"skip_special_tokens,omitempty"`
	TruncatePromptTokens   *int     `json:"truncate_prompt_tokens,omitempty"`

	// Meta / orchestration.
	System   *string `json:"system,omitempty"`
	Template *string `json:"template,omitempty"`
	// RPM/TPM cap requests/tokens per minute for this model across all keys.
	// Enforced in-process (single marbor instance, no distributed state) by the
	// proxy; nil means unlimited.
	RPM *int `json:"rpm,omitempty"`
	TPM *int `json:"tpm,omitempty"`
}

// BenchmarkRun is one completed in-dashboard hardware benchmark run: N cold
// samples (model evicted before each) and N warm samples (model resident
// throughout) measured via the marbor's own /v1/chat/completions, same
// methodology as bench/ttft.go and bench/cold-loop.sh.
// ColdP95Ms/ColdP99Ms/WarmP95Ms/WarmP99Ms extend the original P50/min/max with
// tail latency (P408) - nullable, like ColdTPOTP50Ms/WarmTPOTP50Ms below: nil
// means "not computed" (a row persisted before this migration added these
// columns has no real p95/p99 sample data to backfill from), never a
// fabricated 0 - a run that went through the current aggregateSamples always
// gets a real, non-nil value here (R1).
type BenchmarkRun struct {
	ID            int64     `json:"id"`
	Node          string    `json:"node"`
	Model         string    `json:"model"`
	N             int       `json:"n"`
	ColdP50Ms     float64   `json:"cold_p50_ms"`
	ColdMinMs     float64   `json:"cold_min_ms"`
	ColdMaxMs     float64   `json:"cold_max_ms"`
	ColdP95Ms     *float64  `json:"cold_p95_ms,omitempty"`
	ColdP99Ms     *float64  `json:"cold_p99_ms,omitempty"`
	WarmP50Ms     float64   `json:"warm_p50_ms"`
	WarmMinMs     float64   `json:"warm_min_ms"`
	WarmMaxMs     float64   `json:"warm_max_ms"`
	WarmP95Ms     *float64  `json:"warm_p95_ms,omitempty"`
	WarmP99Ms     *float64  `json:"warm_p99_ms,omitempty"`
	ColdTPOTP50Ms *float64  `json:"cold_tpot_p50_ms,omitempty"`
	WarmTPOTP50Ms *float64  `json:"warm_tpot_p50_ms,omitempty"`
	SpeedupX      float64   `json:"speedup_x"`
	CreatedAt     time.Time `json:"created_at"`
}

// NopStore satisfies Store with all no-ops. Used when db_path = "-".
type NopStore struct{}

func (NopStore) AppendRequest(_ RequestRecord) error                    { return nil }
func (NopStore) LastRequests(_ int) ([]RequestRecord, error)            { return nil, nil }
func (NopStore) GetRequest(_ string) (RequestRecord, bool, error)       { return RequestRecord{}, false, nil }
func (NopStore) UpsertHourlyBucket(_ HourlyBucket) error                { return nil }
func (NopStore) HourlyBuckets(_ time.Time) ([]HourlyBucket, error)      { return nil, nil }
func (NopStore) UpsertModelStat(_ ModelStat) error                      { return nil }
func (NopStore) AllModelStats() ([]ModelStat, error)                    { return nil, nil }
func (NopStore) SetCounters(_ Counters) error                           { return nil }
func (NopStore) GetCounters() (Counters, error)                         { return Counters{}, nil }
func (NopStore) SaveKeyCounters(_ string, _ KeyCounterSnapshot) error   { return nil }
func (NopStore) AllKeyCounters() (map[string]KeyCounterSnapshot, error) { return nil, nil }
func (NopStore) UpsertNode(_ NodeRecord) error                          { return nil }
func (NopStore) DeleteNode(_ string) error                              { return nil }
func (NopStore) AllNodes() ([]NodeRecord, error)                        { return nil, nil }
func (NopStore) UpdateNodeURL(_ string, _ string) error                 { return nil }
func (NopStore) UpsertNodeOverride(_ string, _ *int64, _ *string, _ *string, _ *[]int, _ *int, _ *string, _ *string, _ *int) error {
	return nil
}
func (NopStore) NodeOverrides() (map[string]NodeOverride, error)     { return nil, nil }
func (NopStore) SetNodeDrain(_ string, _ bool, _ string) error       { return nil }
func (NopStore) NodeDrainStates() (map[string]NodeDrainState, error) { return nil, nil }
func (NopStore) UpsertMarborAgent(_ MarborAgentRecord) error         { return nil }
func (NopStore) GetMarborAgent(_ string) (MarborAgentRecord, bool, error) {
	return MarborAgentRecord{}, false, nil
}
func (NopStore) AllMarborAgents() ([]MarborAgentRecord, error)                { return nil, nil }
func (NopStore) DeleteMarborAgent(_ string) error                             { return nil }
func (NopStore) UpsertNodeControlDiscovered(_, _, _ string, _ []string) error { return nil }
func (NopStore) UpsertNodeControlConfigured(_, _, _, _ string) error          { return nil }
func (NopStore) ClearNodeControlConfigured(_ string) error                    { return nil }
func (NopStore) GetNodeControl(_ string) (NodeControlRecord, bool, error) {
	return NodeControlRecord{}, false, nil
}
func (NopStore) AllNodeControl() ([]NodeControlRecord, error)              { return nil, nil }
func (NopStore) DeleteNodeControl(_ string) error                          { return nil }
func (NopStore) AppendPredictiveTransition(_, _ string, _ time.Time) error { return nil }
func (NopStore) PredictiveHistory() ([]PredictiveTransition, error)        { return nil, nil }
func (NopStore) UpsertKey(_ KeyRecord) error                               { return nil }
func (NopStore) RevokeKey(_ string) error                                  { return nil }
func (NopStore) DeleteKey(_ string) error                                  { return nil }
func (NopStore) AllKeys() ([]KeyRecord, error)                             { return nil, nil }
func (NopStore) KeySpendSince(_ string, _ time.Time) (float64, error)      { return 0, nil }
func (NopStore) IncrSpillCounter(_, _ string) error                        { return nil }
func (NopStore) SpillCounters() ([]SpillCounterRow, error)                 { return nil, nil }
func (NopStore) AppendAuditLog(_ AuditEntry) error                         { return nil }
func (NopStore) QueryAuditLog(_ AuditQuery) ([]AuditEntry, error)          { return nil, nil }
func (NopStore) PruneAuditLog(_ int) error                                 { return nil }
func (NopStore) PruneSystemAuditLog(_ int) error                           { return nil }
func (NopStore) AppendSystemAuditLog(_ SystemAuditEntry) error             { return nil }
func (NopStore) QuerySystemAuditLog(_ int) ([]SystemAuditEntry, error)     { return nil, nil }
func (NopStore) QuerySystemAuditLogFiltered(_ SystemAuditFilter) ([]SystemAuditEntry, error) {
	return nil, nil
}
func (NopStore) GetAdminCreds() (AdminCreds, error)                 { return AdminCreds{}, ErrNoAdminCreds }
func (NopStore) SetAdminCreds(_ AdminCreds) error                   { return nil }
func (NopStore) CreateSession(_ string, _ time.Time) error          { return nil }
func (NopStore) ValidateSession(_ string) (bool, error)             { return false, nil }
func (NopStore) DeleteSession(_ string) error                       { return nil }
func (NopStore) PruneExpiredSessions() error                        { return nil }
func (NopStore) CreateUser(_ User) (int64, error)                   { return 0, nil }
func (NopStore) GetUserByUsername(_ string) (User, error)           { return User{}, ErrUserNotFound }
func (NopStore) GetUserByID(_ int64) (User, error)                  { return User{}, ErrUserNotFound }
func (NopStore) ListUsers() ([]User, error)                         { return nil, nil }
func (NopStore) UpdateUser(_ User) error                            { return nil }
func (NopStore) DeleteUser(_ int64) error                           { return nil }
func (NopStore) SoftDeleteUser(_ int64, _ string) error             { return nil }
func (NopStore) CountAdminUsers() (int, error)                      { return 0, nil }
func (NopStore) PendingUserCount() (int, error)                     { return 0, nil }
func (NopStore) CreateUserSession(_ UserSession) error              { return nil }
func (NopStore) GetUserSession(_ string) (UserSession, bool, error) { return UserSession{}, false, nil }
func (NopStore) DeleteUserSession(_ string) error                   { return nil }
func (NopStore) DeleteUserSessionsByUserID(_ int64) error           { return nil }
func (NopStore) PruneExpiredUserSessions() error                    { return nil }
func (NopStore) HasAdminCredentials() (bool, error)                 { return false, nil }
func (NopStore) GetLegacyAdminCreds() (string, string, string, error) {
	return "", "", "", ErrNoAdminCreds
}
func (NopStore) UpsertRoutingRule(_ RoutingRuleRecord) error       { return nil }
func (NopStore) DeleteRoutingRule(_ string) error                  { return nil }
func (NopStore) SetRoutingRuleEnabled(_ string, _ bool) error      { return nil }
func (NopStore) AllRoutingRules() ([]RoutingRuleRecord, error)     { return nil, nil }
func (NopStore) GetSetting(_ string) (string, error)               { return "", ErrNotFound }
func (NopStore) SetSetting(_, _ string) error                      { return nil }
func (NopStore) AllSettings() (map[string]string, error)           { return nil, nil }
func (NopStore) UpsertCloudProvider(_ CloudProviderRecord) error   { return nil }
func (NopStore) DeleteCloudProvider(_ string) error                { return nil }
func (NopStore) AllCloudProviders() ([]CloudProviderRecord, error) { return nil, nil }
func (NopStore) SetCloudProviderPriorities(_ []string) error       { return nil }
func (NopStore) AddFavorite(_ int64, _ string) error               { return nil }
func (NopStore) RemoveFavorite(_ int64, _ string) error            { return nil }
func (NopStore) ListFavorites(_ int64) ([]string, error)           { return nil, nil }
func (NopStore) GetWarmupConfig() (WarmupConfigRecord, error) {
	return WarmupConfigRecord{}, ErrNotFound
}
func (NopStore) SetWarmupConfig(_ bool, _ string) error            { return nil }
func (NopStore) UpsertWarmupModel(_ string, _ []string) error      { return nil }
func (NopStore) DeleteWarmupModel(_ string) error                  { return nil }
func (NopStore) AllWarmupModels() ([]WarmupModelRecord, error)     { return nil, nil }
func (NopStore) RecordWarmLoad(_ WarmStateRecord) error            { return nil }
func (NopStore) SnapshotWarmState(_ WarmStateRecord) error         { return nil }
func (NopStore) DeleteWarmState(_, _ string) error                 { return nil }
func (NopStore) DeleteWarmStateByNode(_ string) error              { return nil }
func (NopStore) AllWarmState() ([]WarmStateRecord, error)          { return nil, nil }
func (NopStore) SnapshotAffinity(_ []AffinityRecord) error         { return nil }
func (NopStore) AllAffinity() ([]AffinityRecord, error)            { return nil, nil }
func (NopStore) ReconcileNodeWarmState(_ string, _ []string) error { return nil }
func (NopStore) GetModelConfig(_, _ string) (ModelConfig, error)   { return ModelConfig{}, ErrNotFound }
func (NopStore) SetModelConfig(_ ModelConfig) error                { return nil }
func (NopStore) DeleteModelConfig(_, _ string) error               { return nil }
func (NopStore) AllModelConfigs() ([]ModelConfig, error)           { return nil, nil }
func (NopStore) InsertBenchmarkRun(_ BenchmarkRun) error           { return nil }
func (NopStore) ListBenchmarkRuns(_ int) ([]BenchmarkRun, error)   { return nil, nil }
func (NopStore) BackupTo(_ string) error {
	return errors.New("store: persistence disabled (db_path=\"-\"), there is no database to back up")
}
func (NopStore) Close() error { return nil }
