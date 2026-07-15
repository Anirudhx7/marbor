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

// Store is the persistence interface. A nil Store is valid (all ops are no-ops)
// so callers never need to nil-check.
type Store interface {
	// Request log
	AppendRequest(r RequestRecord) error
	LastRequests(n int) ([]RequestRecord, error)

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

	// Node overrides (vram, gpu_model)
	UpsertNodeOverride(name string, vramTotalMB *int64, gpuModel *string, runtime *string) error
	NodeOverrides() (map[string]NodeOverride, error)

	// Node drain state
	SetNodeDrain(name string, draining bool, reason string) error
	NodeDrainStates() (map[string]NodeDrainState, error)

	// Predictive engine transition history (survives restart)
	AppendPredictiveTransition(fromModel, toModel string, ts time.Time) error
	PredictiveHistory() ([]PredictiveTransition, error)

	// Runtime API keys (survive restart)
	UpsertKey(k KeyRecord) error
	RevokeKey(name string) error
	AllKeys() ([]KeyRecord, error)
	// KeySpendSince sums real cloud-fallback cost_usd for keyName since the
	// given time, for per-key cloud spend cap checks.
	KeySpendSince(keyName string, since time.Time) (float64, error)

	// Audit log (replaces audit/audit.go file-based logger)
	AppendAuditLog(e AuditEntry) error
	QueryAuditLog(opts AuditQuery) ([]AuditEntry, error)
	PruneAuditLog(retentionDays int) error
	PruneSystemAuditLog(retentionDays int) error

	// System audit log (administrative mutations)
	AppendSystemAuditLog(e SystemAuditEntry) error
	QuerySystemAuditLog(limit int) ([]SystemAuditEntry, error)

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

	// Model configuration overrides - an operator-declared default parameter
	// profile (load-time engine params, inference-time sampling defaults, meta
	// fields) for a model on a specific node, applied whenever ollama-mesh
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

	Close() error
}

// RequestRecord mirrors a single request log entry.
type RequestRecord struct {
	ID         string    `json:"id"`
	KeyName    string    `json:"key_name"`
	Model      string    `json:"model"`
	NodeName   string    `json:"node_name"`
	StatusCode int       `json:"status_code"`
	LatencyMs  int64     `json:"latency_ms"`
	TokensUsed int64     `json:"tokens_used"`
	CostUSD    float64   `json:"cost_usd"`
	RoutedTo   string    `json:"routed_to"`
	IsCloud    bool      `json:"is_cloud"`
	TS         time.Time `json:"ts"`
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
}

// NodeOverride holds operator-declared overrides for a node.
type NodeOverride struct {
	VRAMTotalMB *int64  `json:"vram_total_mb,omitempty"`
	GPUModel    *string `json:"gpu_model,omitempty"`
	Runtime     *string `json:"runtime,omitempty"`
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
	Name          string   `json:"name"`
	Key           string   `json:"key"`
	RateLimit     int      `json:"rate_limit"`
	DailyLimit    int      `json:"daily_limit"`
	MonthlyLimit  int      `json:"monthly_limit"`
	DailyUsdCap   float64  `json:"daily_usd_cap"`
	MonthlyUsdCap float64  `json:"monthly_usd_cap"`
	Models        []string `json:"models"`
	Revoked       bool     `json:"revoked"`
}

// AuditEntry is one structured audit log record persisted to SQLite.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	KeyName    string    `json:"key_name"`
	Model      string    `json:"model"`
	Node       string    `json:"node"`
	Status     string    `json:"status"`
	LatencyMs  int       `json:"latency_ms"`
	Cloud      bool      `json:"cloud"`
	CloudModel string    `json:"cloud_model,omitempty"`
}

// SystemAuditEntry is one administrative mutation event persisted to SQLite.
type SystemAuditEntry struct {
	Time     time.Time `json:"time"`
	Username string    `json:"username"`
	Action   string    `json:"action"`
	Target   string    `json:"target"`
	Details  string    `json:"details"`
	SourceIP string    `json:"source_ip"`
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
	CreatedAt          time.Time  `json:"created_at"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	ApprovedBy         string     `json:"approved_by,omitempty"`
	DeletedAt          *time.Time `json:"deleted_at,omitempty"`
	DeletedBy          string     `json:"deleted_by,omitempty"`
}

// UserSession is an authenticated session tied to a specific User row.
type UserSession struct {
	Token              string
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

// ModelConfig is the operator-declared default parameter profile for a model  --
// covering Ollama's load-time engine params, inference-time sampling defaults,
// and ollama-mesh's own meta/orchestration fields (system prompt override,
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
	// no separate evict-then-reload step is needed on the mesh side. This list
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
	// Enforced in-process (single mesh instance, no distributed state) by the
	// proxy; nil means unlimited.
	RPM *int `json:"rpm,omitempty"`
	TPM *int `json:"tpm,omitempty"`
}

// NopStore satisfies Store with all no-ops. Used when db_path = "-".
type NopStore struct{}

func (NopStore) AppendRequest(_ RequestRecord) error                               { return nil }
func (NopStore) LastRequests(_ int) ([]RequestRecord, error)                       { return nil, nil }
func (NopStore) UpsertHourlyBucket(_ HourlyBucket) error                           { return nil }
func (NopStore) HourlyBuckets(_ time.Time) ([]HourlyBucket, error)                 { return nil, nil }
func (NopStore) UpsertModelStat(_ ModelStat) error                                 { return nil }
func (NopStore) AllModelStats() ([]ModelStat, error)                               { return nil, nil }
func (NopStore) SetCounters(_ Counters) error                                      { return nil }
func (NopStore) GetCounters() (Counters, error)                                    { return Counters{}, nil }
func (NopStore) SaveKeyCounters(_ string, _ KeyCounterSnapshot) error              { return nil }
func (NopStore) AllKeyCounters() (map[string]KeyCounterSnapshot, error)            { return nil, nil }
func (NopStore) UpsertNode(_ NodeRecord) error                                     { return nil }
func (NopStore) DeleteNode(_ string) error                                         { return nil }
func (NopStore) AllNodes() ([]NodeRecord, error)                                   { return nil, nil }
func (NopStore) UpsertNodeOverride(_ string, _ *int64, _ *string, _ *string) error { return nil }
func (NopStore) NodeOverrides() (map[string]NodeOverride, error)                   { return nil, nil }
func (NopStore) SetNodeDrain(_ string, _ bool, _ string) error                     { return nil }
func (NopStore) NodeDrainStates() (map[string]NodeDrainState, error)               { return nil, nil }
func (NopStore) AppendPredictiveTransition(_, _ string, _ time.Time) error         { return nil }
func (NopStore) PredictiveHistory() ([]PredictiveTransition, error)                { return nil, nil }
func (NopStore) UpsertKey(_ KeyRecord) error                                       { return nil }
func (NopStore) RevokeKey(_ string) error                                          { return nil }
func (NopStore) AllKeys() ([]KeyRecord, error)                                     { return nil, nil }
func (NopStore) KeySpendSince(_ string, _ time.Time) (float64, error)              { return 0, nil }
func (NopStore) AppendAuditLog(_ AuditEntry) error                                 { return nil }
func (NopStore) QueryAuditLog(_ AuditQuery) ([]AuditEntry, error)                  { return nil, nil }
func (NopStore) PruneAuditLog(_ int) error                                         { return nil }
func (NopStore) PruneSystemAuditLog(_ int) error                                   { return nil }
func (NopStore) AppendSystemAuditLog(_ SystemAuditEntry) error                     { return nil }
func (NopStore) QuerySystemAuditLog(_ int) ([]SystemAuditEntry, error)             { return nil, nil }
func (NopStore) GetAdminCreds() (AdminCreds, error)                                { return AdminCreds{}, ErrNoAdminCreds }
func (NopStore) SetAdminCreds(_ AdminCreds) error                                  { return nil }
func (NopStore) CreateSession(_ string, _ time.Time) error                         { return nil }
func (NopStore) ValidateSession(_ string) (bool, error)                            { return false, nil }
func (NopStore) DeleteSession(_ string) error                                      { return nil }
func (NopStore) PruneExpiredSessions() error                                       { return nil }
func (NopStore) CreateUser(_ User) (int64, error)                                  { return 0, nil }
func (NopStore) GetUserByUsername(_ string) (User, error)                          { return User{}, ErrUserNotFound }
func (NopStore) GetUserByID(_ int64) (User, error)                                 { return User{}, ErrUserNotFound }
func (NopStore) ListUsers() ([]User, error)                                        { return nil, nil }
func (NopStore) UpdateUser(_ User) error                                           { return nil }
func (NopStore) DeleteUser(_ int64) error                                          { return nil }
func (NopStore) SoftDeleteUser(_ int64, _ string) error                            { return nil }
func (NopStore) CountAdminUsers() (int, error)                                     { return 0, nil }
func (NopStore) PendingUserCount() (int, error)                                    { return 0, nil }
func (NopStore) CreateUserSession(_ UserSession) error                             { return nil }
func (NopStore) GetUserSession(_ string) (UserSession, bool, error)                { return UserSession{}, false, nil }
func (NopStore) DeleteUserSession(_ string) error                                  { return nil }
func (NopStore) DeleteUserSessionsByUserID(_ int64) error                          { return nil }
func (NopStore) PruneExpiredUserSessions() error                                   { return nil }
func (NopStore) HasAdminCredentials() (bool, error)                                { return false, nil }
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
func (NopStore) ReconcileNodeWarmState(_ string, _ []string) error { return nil }
func (NopStore) GetModelConfig(_, _ string) (ModelConfig, error)   { return ModelConfig{}, ErrNotFound }
func (NopStore) SetModelConfig(_ ModelConfig) error                { return nil }
func (NopStore) DeleteModelConfig(_, _ string) error               { return nil }
func (NopStore) AllModelConfigs() ([]ModelConfig, error)           { return nil, nil }
func (NopStore) Close() error                                      { return nil }
