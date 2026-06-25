package store

import "time"

// Store is the persistence interface. A nil Store is valid (all ops are no-ops)
// so callers never need to nil-check.
type Store interface {
	// Request log
	AppendRequest(r RequestRecord) error
	LastRequests(n int) ([]RequestRecord, error)

	// Analytics
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
	UpsertNodeOverride(name string, vramTotalMB *int64, gpuModel *string) error
	NodeOverrides() (map[string]NodeOverride, error)

	// Node drain state
	SetNodeDrain(name string, draining bool) error
	NodeDrainStates() (map[string]bool, error)

	// Runtime API keys (survive restart)
	UpsertKey(k KeyRecord) error
	RevokeKey(name string) error
	AllKeys() ([]KeyRecord, error)

	// Audit log (replaces audit/audit.go file-based logger)
	AppendAuditLog(e AuditEntry) error
	QueryAuditLog(opts AuditQuery) ([]AuditEntry, error)

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
}

// KeyRecord is a runtime API key that should survive a process restart.
type KeyRecord struct {
	Name         string   `json:"name"`
	Key          string   `json:"key"`
	RateLimit    int      `json:"rate_limit"`
	DailyLimit   int      `json:"daily_limit"`
	MonthlyLimit int      `json:"monthly_limit"`
	Models       []string `json:"models"`
	Revoked      bool     `json:"revoked"`
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

// AuditQuery controls filtering for QueryAuditLog.
type AuditQuery struct {
	Limit int
	Model string
	Key   string
	Cloud *bool
	Since time.Time
}

// NopStore satisfies Store with all no-ops. Used when db_path = "-".
type NopStore struct{}

func (NopStore) AppendRequest(_ RequestRecord) error                    { return nil }
func (NopStore) LastRequests(_ int) ([]RequestRecord, error)            { return nil, nil }
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
func (NopStore) UpsertNodeOverride(_ string, _ *int64, _ *string) error { return nil }
func (NopStore) NodeOverrides() (map[string]NodeOverride, error)        { return nil, nil }
func (NopStore) SetNodeDrain(_ string, _ bool) error                    { return nil }
func (NopStore) NodeDrainStates() (map[string]bool, error)              { return nil, nil }
func (NopStore) UpsertKey(_ KeyRecord) error                            { return nil }
func (NopStore) RevokeKey(_ string) error                               { return nil }
func (NopStore) AllKeys() ([]KeyRecord, error)                          { return nil, nil }
func (NopStore) AppendAuditLog(_ AuditEntry) error                      { return nil }
func (NopStore) QueryAuditLog(_ AuditQuery) ([]AuditEntry, error)       { return nil, nil }
func (NopStore) Close() error                                           { return nil }
