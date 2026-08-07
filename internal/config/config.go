package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"runtime"
	"strings"
	"time"
	_ "time/tzdata"
)

// ValidateNodeURL checks that raw is a usable http(s) backend URL and rejects
// link-local / cloud-metadata hosts (169.254.0.0/16 including the
// 169.254.169.254 metadata endpoint, and fe80::/10) so an operator- or
// API-supplied node cannot turn the mesh into an SSRF relay to the host's
// metadata service. Loopback and RFC1918 private ranges are intentionally
// ALLOWED: homelab and on-prem fleets legitimately run backends on localhost
// and LAN addresses (and the test suite uses 127.0.0.1). Hostnames are not
// resolved here; the literal-IP guard stops the practical add-a-metadata-node
// attack without breaking DNS-named LAN backends.
func ValidateNodeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("URL must be http(s) with a host: %s", raw)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("URL host %q is a link-local/metadata address, which is not allowed", u.Hostname())
		}
	}
	return nil
}

// NormalizeNodeURL returns a canonical form of a node backend URL suitable for
// identity comparison: lowercase scheme and host (so "HTTP://Host:11434" and
// "http://host:11434" compare equal) with any trailing slash on the path
// stripped (so "http://host:11434" and "http://host:11434/" compare equal).
// Query strings and fragments are dropped since a backend URL should never
// carry them; this keeps the comparison focused on "same physical endpoint".
// If raw does not parse as a URL, the lowercased and trailing-slash-trimmed
// raw string is returned as a best-effort fallback so callers always get a
// usable comparison key instead of an error.
func NormalizeNodeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	path := strings.TrimRight(u.Path, "/")
	return scheme + "://" + host + path
}

// WarmupEntry names a model to keep warm and optionally restricts which nodes
// receive keepalive pings. Empty Nodes means "all healthy nodes".
type WarmupEntry struct {
	Model string   `yaml:"model" json:"model"`
	Nodes []string `yaml:"nodes,omitempty" json:"nodes,omitempty"`
}

// WarmupConfig controls proactive model keepalive. The warmer sends a
// zero-token /api/generate with keep_alive to each configured node on
// IntervalMs cadence so models never get evicted from VRAM between requests.
type WarmupConfig struct {
	Enabled    bool `yaml:"enabled" json:"enabled"`
	IntervalMs int  `yaml:"interval_ms" json:"interval_ms"`
	// KeepAlive is the Ollama keep_alive value forwarded in each ping (e.g.
	// "10m"). Must exceed IntervalMs; defaults to "10m".
	KeepAlive string        `yaml:"keep_alive" json:"keep_alive"`
	Models    []WarmupEntry `yaml:"models,omitempty" json:"models,omitempty"`
}

type Config struct {
	Timezone         string            `yaml:"timezone" json:"timezone"`
	Proxy            ProxyConfig       `yaml:"proxy"`
	Admin            AdminConfig       `yaml:"admin" json:"admin"`
	Auth             AuthConfig        `yaml:"auth"`
	Nodes            []NodeConfig      `yaml:"nodes"`
	Routing          RoutingConfig     `yaml:"routing"`
	Metrics          MetricsConfig     `yaml:"metrics"`
	LiteLLM          LiteLLMConfig     `yaml:"litellm"`
	CloudProviders   []CloudProvider   `yaml:"cloud_providers" json:"cloud_providers"`
	Docker           DockerConfig      `yaml:"docker" json:"docker"`
	Audit            AuditConfig       `yaml:"audit" json:"audit"`
	Webhook          WebhookConfig     `yaml:"webhook" json:"webhook"`
	Savings          SavingsConfig     `yaml:"savings" json:"savings"`
	Warmup           WarmupConfig      `yaml:"warmup" json:"warmup"`
	Backup           BackupConfig      `yaml:"backup" json:"backup"`
	HuggingFace      HuggingFaceConfig `yaml:"huggingface" json:"huggingface"`
	CloudBudget      CloudBudgetConfig `yaml:"cloud_budget" json:"cloud_budget"`
	HideDemoBanner   bool              `yaml:"hide_demo_banner" json:"hide_demo_banner"`
	HideBudgetBanner bool              `yaml:"hide_budget_banner" json:"hide_budget_banner"`
	// ContextWindows maps a model name to its max context window in tokens.
	// Operator-declared, like a node's vram_total_mb - never guessed. A model
	// absent from this map has no admission-time context-length check.
	ContextWindows map[string]int `yaml:"context_windows" json:"context_windows"`
}

// CloudBudgetConfig caps total cloud-fallback spend using the real CostUSD
// already persisted per hourly bucket. Both caps default to 0 (disabled) -
// an absent or zero cap never blocks cloud fallback, preserving existing
// behavior for anyone who hasn't opted in.
type CloudBudgetConfig struct {
	// DailyUSDCap blocks new cloud-fallback dispatch once today's (UTC)
	// cumulative cloud spend reaches this amount. 0 disables the check.
	DailyUSDCap float64 `yaml:"daily_usd_cap" json:"daily_usd_cap"`
	// MonthlyUSDCap blocks new cloud-fallback dispatch once this calendar
	// month's (UTC) cumulative cloud spend reaches this amount. 0 disables
	// the check.
	MonthlyUSDCap float64 `yaml:"monthly_usd_cap" json:"monthly_usd_cap"`
	// SoftBudgetPct is the fraction (0-1) of either cap at which a warning
	// should surface without blocking cloud fallback. 0 disables the warning.
	SoftBudgetPct float64 `yaml:"soft_budget_pct,omitempty" json:"soft_budget_pct,omitempty"`
}

type HuggingFaceConfig struct {
	Token string `yaml:"token" json:"token"`
}

// BackupConfig controls the scheduled mesh.db backup job (P49). TargetDir is
// seeded from the MESH_BACKUP_DIR env var (or a "backups" dir next to the
// database) before an operator ever opens Settings, so an out-of-the-box
// Docker deployment backs up correctly with zero configuration - see
// docker-compose.yml's separate "mesh-backups" volume, kept distinct from the
// "mesh-data" volume so wiping one doesn't take out the other.
type BackupConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// IntervalHours is how often a scheduled backup runs. Defaults to 24 (see
	// Validate()).
	IntervalHours int `yaml:"interval_hours" json:"interval_hours"`
	// RetentionCount is how many scheduled backup files to keep in TargetDir;
	// older files beyond this count are deleted after each successful run.
	// Defaults to 7.
	RetentionCount int `yaml:"retention_count" json:"retention_count"`
	// TargetDir is the directory scheduled backups are written to.
	TargetDir string `yaml:"target_dir" json:"target_dir"`
	// LastBackupAt/LastBackupError are read-only status populated by the admin
	// server at GET time (never accepted on PUT, never persisted under these
	// field names) - R1: the UI shows the real last-run outcome, never a
	// fabricated "backed up" status.
	LastBackupAt    string `yaml:"-" json:"last_backup_at,omitempty"`
	LastBackupError string `yaml:"-" json:"last_backup_error,omitempty"`
}

// SavingsConfig controls how locally-served tokens are valued in the
// dashboard savings calculation.
type SavingsConfig struct {
	// ReferenceCostPer1K is the cloud rate (USD per 1K tokens) used to value
	// tokens served by local nodes. Defaults to 0.002 when unset.
	ReferenceCostPer1K float64 `yaml:"reference_cost_per_1k" json:"reference_cost_per_1k"`
}

type WebhookConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	URL     string `yaml:"url" json:"url"`
	// Secret is used to compute an HMAC-SHA256 signature sent as X-Ollama-Mesh-Signature.
	Secret string `yaml:"secret" json:"secret"`
}

type AuditConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// RetentionDays bounds how long the per-request audit_log is kept before
	// a periodic prune deletes old rows. Admin-configurable: compliance-
	// driven operators want months, disk-constrained self-hosted shops want
	// days. 0 is a deliberate "keep forever" choice (see Validate()).
	RetentionDays int `yaml:"retention_days" json:"retention_days"`
	// SystemAuditRetentionDays bounds the separate system_audit_log (admin
	// action trail - who changed what). Kept independent of RetentionDays:
	// this table is low-volume and security-sensitive, so it defaults to 0
	// (forever) rather than the per-request log's 30-day default.
	SystemAuditRetentionDays int `yaml:"system_audit_retention_days" json:"system_audit_retention_days"`
}

type DockerConfig struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	Socket         string `yaml:"socket" json:"socket"`
	PollIntervalMs int    `yaml:"poll_interval_ms" json:"poll_interval_ms"`
}

type ProxyConfig struct {
	Port int `yaml:"port"`
	// LogFormat controls the format of system (non-access) log lines.
	// "text" emits the default Go log prefix lines; "json" emits slog JSON
	// objects that log aggregators (Loki, Datadog, Fluentd) can parse natively.
	LogFormat string `yaml:"log_format" json:"log_format"`
	// AccessLog enables a structured JSON access-log line on stdout per request.
	// Defaults to true (enabled) when unset. Set to false to silence it.
	AccessLog *bool `yaml:"access_log,omitempty" json:"access_log,omitempty"`
	// TrustProxyHeaders controls whether the proxy trusts client-supplied
	// X-Forwarded-For/X-Real-IP headers when recording the "source IP" on the
	// admin request log. Defaults to false: any directly-reachable client can
	// forge these headers, so the mesh logs r.RemoteAddr (the real TCP peer)
	// unless an operator explicitly confirms requests only ever arrive through
	// a trusted reverse proxy/load balancer that sets these headers itself.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers,omitempty" json:"trust_proxy_headers,omitempty"`
}

// AdminConfig controls the admin dashboard/API listener.
type AdminConfig struct {
	// BindAddress is the listen address for the admin server. Defaults to
	// ":8080" (all interfaces) for backward compatibility and Docker port
	// mapping. For a hardened single-host deploy, set "127.0.0.1:8080" so the
	// dashboard is not reachable from the network.
	BindAddress string `yaml:"bind_address,omitempty" json:"bind_address,omitempty"`
	// CORSOrigin is the value sent in Access-Control-Allow-Origin on admin API
	// responses. Empty (default) sends no CORS headers, so the API is only
	// callable same-origin (the embedded dashboard). Set a specific origin to
	// allow a separate front-end. The admin session is an httpOnly cookie, so
	// a non-empty origin also gets Access-Control-Allow-Credentials: true
	// paired with it - "*" will NOT work here (browsers reject a wildcard
	// origin combined with credentials), so this must be one concrete origin.
	CORSOrigin string `yaml:"cors_origin,omitempty" json:"cors_origin,omitempty"`
}

type AuthConfig struct {
	// Enabled turns auth enforcement on. Defaults to true (enabled) when the
	// key is absent from the config file; set to false to explicitly disable.
	// Read via IsEnabled(), never the field directly.
	Enabled *bool       `yaml:"enabled,omitempty"`
	Keys    []KeyConfig `yaml:"keys"`
}

type KeyConfig struct {
	Name      string   `yaml:"name" json:"name"`
	Key       string   `yaml:"key" json:"key"`
	RateLimit int      `yaml:"rate_limit" json:"rateLimit"`
	Models    []string `yaml:"models,omitempty" json:"models,omitempty"`
	ExpiresAt string   `yaml:"expires_at,omitempty" json:"expiresAt,omitempty"`
	// DailyLimit and MonthlyLimit are hard request quotas per key for the
	// current day/month. 0 means unlimited. Exceeding either returns 429.
	DailyLimit   int `yaml:"daily_limit,omitempty" json:"dailyLimit,omitempty"`
	MonthlyLimit int `yaml:"monthly_limit,omitempty" json:"monthlyLimit,omitempty"`
	// DailyUsdCap and MonthlyUsdCap are per-key cloud-fallback spend caps in
	// USD, distinct from the request-count limits above. 0 means unlimited.
	// Checked the same way as the global CloudBudgetConfig caps, against this
	// key's real cost_usd in request_log.
	DailyUsdCap   float64 `yaml:"daily_usd_cap,omitempty" json:"dailyUsdCap,omitempty"`
	MonthlyUsdCap float64 `yaml:"monthly_usd_cap,omitempty" json:"monthlyUsdCap,omitempty"`
	// LocalOnly, when true, forbids cloud fallback for this key entirely: a
	// request that would otherwise spill to a cloud provider instead fails
	// closed with an explicit error, so the key's traffic never leaves local
	// nodes. Default false preserves today's fallback behavior for every
	// existing key.
	LocalOnly bool `yaml:"local_only,omitempty" json:"localOnly,omitempty"`
}

type NodeConfig struct {
	Name        string `yaml:"name" json:"name"`
	URL         string `yaml:"url" json:"url"`
	GPUModel    string `yaml:"gpu_model" json:"gpu_model"`
	NvidiaIndex int    `yaml:"nvidia_index" json:"nvidia_index"`
	// VRAMTotalMB optionally declares this node's total GPU VRAM in MB. Used to
	// compute headroom for remote nodes where nvidia-smi cannot reach (nvidia-smi
	// only sees the mesh host). Operator-declared, surfaced as "declared", never
	// presented as a live measurement. 0 = unknown (UI shows capacity as "-").
	VRAMTotalMB int64 `yaml:"vram_total_mb" json:"vram_total_mb"`
	// Runtime identifies the inference backend. Valid: "ollama" (default), "vllm", "tgi", "llamacpp", "mlx".
	// Controls which health endpoint and warm-model detection API the router uses.
	Runtime string `yaml:"runtime" json:"runtime"`
	// VRAMOverrides declares, per model name, how much VRAM (MB) that model
	// consumes on this node. Non-Ollama runtimes (vllm, tgi, llamacpp, mlx) don't
	// expose per-model VRAM/disk size via their APIs, so without this the
	// router has no way to size a not-yet-loaded model and silently disables
	// predictive warmup and headroom/eviction checks for that node. Operator-
	// declared, like vram_total_mb - never guessed (R1). Empty/absent map is a
	// no-op; only declare sizes you actually know.
	VRAMOverrides map[string]int64 `yaml:"vram_overrides,omitempty" json:"vram_overrides,omitempty"`
	// Host groups this node with any other node that lives on the same
	// physical machine (e.g. Ollama on :11434 and vLLM on :8000 on one box),
	// so they can share one Node Agent enrollment/token instead of each
	// needing its own. Empty defaults to the URL's hostname in
	// Router.AddNode - most operators never need to set this explicitly.
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
}

type RoutingRule struct {
	ID         string `yaml:"id" json:"id"`
	Priority   int    `yaml:"priority" json:"priority"`
	Condition  string `yaml:"condition" json:"condition"`
	TargetNode string `yaml:"target_node" json:"targetNode"`
	Strategy   string `yaml:"strategy" json:"strategy"`
	Enabled    bool   `yaml:"enabled" json:"enabled"`
}

type RoutingConfig struct {
	Strategy       string        `yaml:"strategy" json:"strategy"`
	PollIntervalMs int           `yaml:"poll_interval_ms" json:"poll_interval_ms"`
	Fallback       string        `yaml:"fallback" json:"fallback"`
	Rules          []RoutingRule `yaml:"rules" json:"rules"`
	// UpstreamTimeoutMs bounds how long the proxy waits for an upstream node
	// to send response headers before giving up and trying the next node.
	// Covers the header phase only, never the streaming body. Defaults to 120000.
	UpstreamTimeoutMs int `yaml:"upstream_timeout_ms" json:"upstream_timeout_ms"`
	// MaxRetries is how many alternate healthy nodes the proxy will try when an
	// upstream fails before any response bytes are sent. Defaults to 2.
	MaxRetries int `yaml:"max_retries" json:"max_retries"`
	// AllowManagementEndpoints, when true, lets clients reach Ollama's
	// destructive model-management endpoints (/api/delete, /api/pull, /api/push,
	// /api/create, /api/copy, /api/blobs) through the proxy. Default false
	// (zero-value): those paths are blocked with 403 so an inference key in a
	// multi-tenant deployment cannot mutate models on shared backend nodes. Set
	// true only for single-tenant homelab use. No Validate() default needed -
	// false is the safe zero value.
	AllowManagementEndpoints bool `yaml:"allow_management_endpoints" json:"allow_management_endpoints"`
	// SessionAffinity, when true, routes requests that share an X-Session-ID
	// header to the same backend node. The node's KV cache (context history)
	// from prior turns stays in VRAM, so subsequent requests skip the prefill
	// re-computation and produce the first token faster. Default false (zero-
	// value safe): stateless routing, no sticky sessions.
	SessionAffinity bool `yaml:"session_affinity" json:"session_affinity"`
	// SessionAffinityTTL is a duration string (e.g. "10m", "30m") specifying
	// how long an idle session pin stays valid. After this window with no
	// requests, the next request re-routes normally. Default "10m".
	SessionAffinityTTL string `yaml:"session_affinity_ttl" json:"session_affinity_ttl"`
	// NvidiaPollIntervalMs controls how often the router calls nvidia-smi to
	// refresh VRAM/temperature/power data for local nodes. nvidia-smi forks a
	// subprocess each call, so the default is 30s rather than the /api/ps
	// poll rate (2s) to avoid measurable CPU overhead on the mesh host.
	NvidiaPollIntervalMs int `yaml:"nvidia_poll_interval_ms" json:"nvidia_poll_interval_ms"`
	// QueueMaxDepth is the maximum number of requests that can wait for a local
	// node to become available. When the cluster is fully saturated, requests
	// are queued here before falling through to cloud fallback or 503. 0
	// disables queuing (immediate cloud/503). Default 100.
	QueueMaxDepth int `yaml:"queue_max_depth" json:"queue_max_depth"`
	// QueueTimeoutMs is how long a queued request waits for a node to free up
	// before giving up and falling through to cloud fallback or 503. Default
	// 30000 (30s).
	QueueTimeoutMs int `yaml:"queue_timeout_ms" json:"queue_timeout_ms"`
	// HealthFailureThreshold is how many consecutive failed polls mark a node
	// unhealthy. Default 3 (unchanged from the prior hardcoded behavior).
	HealthFailureThreshold int `yaml:"health_failure_threshold" json:"health_failure_threshold"`
	// HealthSuccessThreshold is how many consecutive successful polls are
	// required to mark a previously-unhealthy node healthy again. Default 2:
	// asymmetric with the failure threshold so a node isn't put back into
	// rotation on a single lucky poll after a real outage (flapping), while
	// still recovering faster than it took to be marked down.
	HealthSuccessThreshold int `yaml:"health_success_threshold" json:"health_success_threshold"`
	// FallbackChains maps a model name to an ordered list of already-downloaded
	// alternates to try when the primary model provably does not fit in free
	// VRAM on any healthy node. Opt-in and empty by default - there is no
	// silent substitution outside a chain the operator explicitly declared,
	// and a candidate is only used if it is already present on a node (never
	// triggers a fresh multi-GB download on the hot path). This is a
	// pre-scoring Hard-Constraint filter; it does not touch weighted
	// placement scoring.
	FallbackChains map[string][]string `yaml:"fallback_chains" json:"fallback_chains"`
	// OverflowSLAMs, when > 0, caps how long a request waits in the local
	// capacity queue before falling through to cloud fallback (or 503),
	// overriding the longer queue_timeout_ms for that purpose only. It never
	// bypasses a genuine Hard Constraint (health, runtime compatibility) -
	// Route() itself is unaffected; this only shortens how long a request
	// waits for local capacity to free up. Default 0 (disabled): the full
	// queue_timeout_ms applies as before.
	OverflowSLAMs int `yaml:"overflow_sla_ms" json:"overflow_sla_ms"`
	// ThermalWatchdog implements "Sustained Degradation Auto-Drain": reuses
	// the already-polled NVIDIA temperature data to auto-drain a node (via
	// the existing DrainNode path) after sustained thermal breach.
	// One-directional - recovery always requires an admin to undrain
	// manually. Disabled by default.
	ThermalWatchdog ThermalWatchdogConfig `yaml:"thermal_watchdog" json:"thermal_watchdog"`
}

// ThermalWatchdogConfig gates the auto-drain-on-sustained-overheat behavior.
type ThermalWatchdogConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxTempCelsius is the temperature threshold. 0 (even with
	// Enabled=true) is treated as disabled - never invent a threshold.
	MaxTempCelsius float64 `yaml:"max_temp_celsius" json:"max_temp_celsius"`
	// ConsecutiveBreaches is how many consecutive polls at/above the
	// threshold trigger a drain. Default 3 (see router.New()).
	ConsecutiveBreaches int `yaml:"consecutive_breaches" json:"consecutive_breaches"`
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Port    int  `yaml:"port" json:"port"`
}

type LiteLLMConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	URL     string `yaml:"url" json:"url"`
	// APIKey is the LiteLLM virtual/master key sent as Authorization: Bearer
	// <key> - required for any real LiteLLM deployment that isn't running
	// with auth disabled.
	APIKey string `yaml:"api_key" json:"api_key"`
}

type CloudProvider struct {
	Name            string  `yaml:"name" json:"name"`
	Provider        string  `yaml:"provider" json:"provider"` // openai, anthropic, groq, together
	BaseURL         string  `yaml:"base_url" json:"base_url"`
	APIKey          string  `yaml:"api_key" json:"api_key"`
	DefaultModel    string  `yaml:"default_model" json:"default_model"`
	CostPer1KTokens float64 `yaml:"cost_per_1k_tokens" json:"cost_per_1k_tokens"`
	Enabled         bool    `yaml:"enabled" json:"enabled"`
	// Priority orders cloud fallback attempts when more than one provider is
	// enabled - higher tries first. Ties fall back to insertion order (the
	// same tie-break SQLite uses: ORDER BY priority DESC, name ASC).
	Priority int `yaml:"priority" json:"priority"`
}

// IsEnabled reports whether auth enforcement is on.
// Defaults to true when the field is absent (nil) from the config file.
func (c AuthConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// BoolPtr returns a pointer to b. Used to construct explicit *bool config
// values (e.g. AuthConfig.Enabled) in code and tests, including from other
// packages.
func BoolPtr(b bool) *bool { return &b }

// warnIfAdminBindsAllInterfaces logs a one-time startup warning when the
// admin dashboard/API listens on every network interface (2026-07-14 audit,
// Priority 6). The default is intentionally kept at ":8080" for Docker port
// mapping compatibility (`-p 8080:8080` requires the process inside the
// container to listen on 0.0.0.0, not 127.0.0.1) - changing the default
// would break the project's own docker-compose.yml. Surfacing this via a
// runtime log line (not just a config file comment) means an operator on an
// untrusted network actually sees it in `docker logs`, not just documentation
// most people don't read.
func warnIfAdminBindsAllInterfaces(bindAddress string) {
	host, _, err := net.SplitHostPort(bindAddress)
	if err != nil {
		host = bindAddress
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		log.Printf("WARNING: admin dashboard is bound to all interfaces (%s). If this host is reachable from an untrusted network, set admin.bind_address to 127.0.0.1:8080 (or the equivalent host-side binding) or place it behind a firewall/reverse proxy.", bindAddress)
	}
}

func (c *Config) Validate() error {
	if c.Timezone == "" {
		c.Timezone = "Local"
	}
	if c.Timezone != "Local" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
		}
	}

	if c.Proxy.Port == 0 {
		c.Proxy.Port = 11434
	}
	if c.Proxy.LogFormat == "" {
		c.Proxy.LogFormat = "text"
	}
	if c.Proxy.AccessLog == nil {
		enabled := true
		c.Proxy.AccessLog = &enabled
	}
	if c.Admin.BindAddress == "" {
		c.Admin.BindAddress = ":8080"
	}
	warnIfAdminBindsAllInterfaces(c.Admin.BindAddress)
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "warm-first"
	}
	if c.Routing.PollIntervalMs == 0 {
		c.Routing.PollIntervalMs = 2000
	}
	if c.Routing.Fallback == "" {
		c.Routing.Fallback = "least-connections"
	}
	if c.Routing.UpstreamTimeoutMs == 0 {
		c.Routing.UpstreamTimeoutMs = 120000
	}
	if c.Routing.MaxRetries == 0 {
		c.Routing.MaxRetries = 2
	}
	if c.Routing.NvidiaPollIntervalMs == 0 {
		c.Routing.NvidiaPollIntervalMs = 30000
	}
	if c.Routing.QueueMaxDepth == 0 {
		c.Routing.QueueMaxDepth = 100
	}
	if c.Routing.QueueTimeoutMs == 0 {
		c.Routing.QueueTimeoutMs = 30000
	}
	if c.Routing.HealthFailureThreshold == 0 {
		c.Routing.HealthFailureThreshold = 3
	}
	if c.Routing.HealthSuccessThreshold == 0 {
		c.Routing.HealthSuccessThreshold = 2
	}
	if c.Metrics.Port == 0 {
		c.Metrics.Port = 9090
	}
	// 0 is a deliberate, valid choice here (keep audit_log rows forever /
	// disable pruning) - unlike other zero-valued fields in this func, it is
	// NOT normalized to a default. The 30-day default only applies on first
	// boot, before any admin choice exists (see main.go's settings overlay).
	if c.Audit.RetentionDays < 0 {
		return fmt.Errorf("audit.retention_days must be >= 0 (0 keeps audit log entries forever)")
	}
	if c.Audit.SystemAuditRetentionDays < 0 {
		return fmt.Errorf("audit.system_audit_retention_days must be >= 0 (0 keeps entries forever)")
	}

	// Auth defaults to enabled: make an absent key explicit so a saved config
	// records the real default rather than relying on nil semantics.
	if c.Auth.Enabled == nil {
		c.Auth.Enabled = BoolPtr(true)
	}
	if c.Auth.IsEnabled() {
		seen := make(map[string]bool)
		for _, k := range c.Auth.Keys {
			if k.Name == "" || k.Key == "" {
				return fmt.Errorf("auth key must have name and key")
			}
			if seen[k.Name] {
				return fmt.Errorf("duplicate auth key name: %s", k.Name)
			}
			seen[k.Name] = true
		}
	}

	seenNodeNames := make(map[string]bool, len(c.Nodes))
	seenNodeURLs := make(map[string]bool, len(c.Nodes))
	for i, n := range c.Nodes {
		if n.Name == "" || n.URL == "" {
			return fmt.Errorf("node %d must have name and url", i)
		}
		if seenNodeNames[n.Name] {
			return fmt.Errorf("duplicate node name: %s", n.Name)
		}
		seenNodeNames[n.Name] = true
		// Validate scheme/host and block link-local/metadata addresses (SSRF).
		// url.Parse is lenient (accepts "garbage"), so fail fast at boot rather
		// than 500ing later. Loopback/RFC1918 stay allowed for homelab/LAN.
		if err := ValidateNodeURL(n.URL); err != nil {
			return fmt.Errorf("node %d (%s): %w", i, n.Name, err)
		}
		normURL := NormalizeNodeURL(n.URL)
		if seenNodeURLs[normURL] {
			return fmt.Errorf("duplicate node URL (same backend under a different name): %s", n.URL)
		}
		seenNodeURLs[normURL] = true
		if c.Nodes[i].Runtime == "" {
			c.Nodes[i].Runtime = "ollama"
		}
		if c.Nodes[i].VRAMOverrides == nil {
			c.Nodes[i].VRAMOverrides = map[string]int64{}
		}
		switch c.Nodes[i].Runtime {
		case "ollama", "vllm", "tgi", "llamacpp", "mlx", "auto":
			// valid
		default:
			return fmt.Errorf("node %s: unknown runtime %q (valid: ollama, vllm, tgi, llamacpp, mlx, auto)", n.Name, c.Nodes[i].Runtime)
		}
	}
	// Detect port collisions between proxy, admin, and metrics servers.
	if c.Metrics.Enabled && c.Metrics.Port == c.Proxy.Port {
		return fmt.Errorf("metrics port %d conflicts with proxy port", c.Metrics.Port)
	}

	if c.Docker.Socket == "" {
		if runtime.GOOS == "windows" {
			c.Docker.Socket = `//./pipe/docker_engine`
		} else {
			c.Docker.Socket = "/var/run/docker.sock"
		}
	}
	if c.Docker.PollIntervalMs == 0 {
		c.Docker.PollIntervalMs = 30000
	}

	if c.Warmup.IntervalMs == 0 {
		c.Warmup.IntervalMs = 300000 // 5 min
	}
	if c.Warmup.KeepAlive == "" {
		c.Warmup.KeepAlive = "10m"
	}

	if c.Backup.IntervalHours == 0 {
		c.Backup.IntervalHours = 24
	}
	if c.Backup.IntervalHours < 0 {
		return fmt.Errorf("backup.interval_hours must be >= 0")
	}
	if c.Backup.RetentionCount == 0 {
		c.Backup.RetentionCount = 7
	}
	if c.Backup.RetentionCount < 0 {
		return fmt.Errorf("backup.retention_count must be >= 0")
	}

	if c.Savings.ReferenceCostPer1K <= 0 {
		c.Savings.ReferenceCostPer1K = 0.002
	}

	if c.LiteLLM.Enabled && c.LiteLLM.URL == "" {
		return fmt.Errorf("litellm URL required when enabled")
	}

	for i, cp := range c.CloudProviders {
		if cp.Enabled {
			if cp.BaseURL == "" || cp.APIKey == "" {
				return fmt.Errorf("cloud provider %d (%s) requires base_url and api_key when enabled", i, cp.Name)
			}
			u, err := url.Parse(cp.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("cloud provider %d (%s) base_url must be http(s) with a host: %s", i, cp.Name, cp.BaseURL)
			}
		}
	}

	return nil
}
