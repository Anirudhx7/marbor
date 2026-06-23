package config

import (
	"fmt"
	"net/url"
	"os"
	"runtime"

	"gopkg.in/yaml.v3"
)

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
	Proxy          ProxyConfig       `yaml:"proxy"`
	Admin          AdminConfig       `yaml:"admin" json:"admin"`
	Auth           AuthConfig        `yaml:"auth"`
	Nodes          []NodeConfig      `yaml:"nodes"`
	Routing        RoutingConfig     `yaml:"routing"`
	Metrics        MetricsConfig     `yaml:"metrics"`
	LiteLLM        LiteLLMConfig     `yaml:"litellm"`
	CloudProviders []CloudProvider   `yaml:"cloud_providers" json:"cloud_providers"`
	Docker         DockerConfig      `yaml:"docker" json:"docker"`
	Audit          AuditConfig       `yaml:"audit" json:"audit"`
	Webhook        WebhookConfig     `yaml:"webhook" json:"webhook"`
	Savings        SavingsConfig     `yaml:"savings" json:"savings"`
	HA             HAConfig          `yaml:"ha" json:"ha"`
	Warmup         WarmupConfig      `yaml:"warmup" json:"warmup"`
	HuggingFace    HuggingFaceConfig `yaml:"huggingface" json:"huggingface"`
}

type HuggingFaceConfig struct {
	Token string `yaml:"token" json:"token"`
}

// HAConfig controls peer-awareness for active/active HA deployments.
// Two mesh instances point at each other's admin port; a TCP LB (HAProxy/nginx)
// sits in front of both. The monitor polls each peer's /health endpoint and
// logs reachability transitions so operators can observe split-brain or partial
// failure without any external monitoring tool.
type HAConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Peers               []string `yaml:"peers" json:"peers"`
	HeartbeatIntervalMs int      `yaml:"heartbeat_interval_ms" json:"heartbeat_interval_ms"`
	PeerTimeoutMs       int      `yaml:"peer_timeout_ms" json:"peer_timeout_ms"`
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
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Path    string `yaml:"path" json:"path"`
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
	// allow a separate front-end; "*" is allowed but not recommended.
	CORSOrigin string `yaml:"cors_origin,omitempty" json:"cors_origin,omitempty"`
}

type AuthConfig struct {
	Enabled bool        `yaml:"enabled"`
	Keys    []KeyConfig `yaml:"keys"`
	// AdminToken is the bearer token for the admin dashboard API.
	// Optional: when empty, the first auth key (or "admin") is used,
	// preserving pre-first-run behavior.
	AdminToken string `yaml:"admin_token,omitempty" json:"-"`
	// StatePath is where per-key usage counters are persisted so quotas and
	// usage survive restarts. Defaults to "usage-state.json". Set to "-" to
	// disable persistence.
	StatePath string `yaml:"state_path,omitempty" json:"-"`
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
}

type NodeConfig struct {
	Name        string `yaml:"name" json:"name"`
	URL         string `yaml:"url" json:"url"`
	GPUModel    string `yaml:"gpu_model" json:"gpu_model"`
	NvidiaIndex int    `yaml:"nvidia_index" json:"nvidia_index"`
	// VRAMTotalMB optionally declares this node's total GPU VRAM in MB. Used to
	// compute headroom for remote nodes where nvidia-smi cannot reach (nvidia-smi
	// only sees the mesh host). Operator-declared, surfaced as "declared", never
	// presented as a live measurement. 0 = unknown (UI shows capacity as "—").
	VRAMTotalMB int64 `yaml:"vram_total_mb" json:"vram_total_mb"`
	// Runtime identifies the inference backend. Valid: "ollama" (default), "vllm", "tgi", "llamacpp".
	// Controls which health endpoint and warm-model detection API the router uses.
	Runtime string `yaml:"runtime" json:"runtime"`
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
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Port    int  `yaml:"port" json:"port"`
}

type LiteLLMConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	URL     string `yaml:"url" json:"url"`
}

type CloudProvider struct {
	Name            string  `yaml:"name" json:"name"`
	Provider        string  `yaml:"provider" json:"provider"` // openai, anthropic, groq, together
	BaseURL         string  `yaml:"base_url" json:"base_url"`
	APIKey          string  `yaml:"api_key" json:"api_key"`
	DefaultModel    string  `yaml:"default_model" json:"default_model"`
	CostPer1KTokens float64 `yaml:"cost_per_1k_tokens" json:"cost_per_1k_tokens"`
	Enabled         bool    `yaml:"enabled" json:"enabled"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func SaveConfig(path string, cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	// os.WriteFile won't chmod an existing file to the new mode, so enforce
	// 0600 explicitly to prevent re-widening when the file pre-exists at 0644.
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	return nil
}

func (c *Config) Validate() error {
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
	if c.Metrics.Port == 0 {
		c.Metrics.Port = 9090
	}

	if c.Auth.StatePath == "" {
		c.Auth.StatePath = "usage-state.json"
	}

	if c.Auth.Enabled {
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
		u, err := url.Parse(n.URL)
		if err != nil {
			return fmt.Errorf("invalid node URL %s: %w", n.URL, err)
		}
		// url.Parse is lenient (it accepts "garbage"); fail fast at boot on a
		// URL that is not a usable http(s) endpoint instead of 500ing later.
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("node %d (%s) URL must be http(s) with a host: %s", i, n.Name, n.URL)
		}
		if seenNodeURLs[n.URL] {
			return fmt.Errorf("duplicate node URL: %s", n.URL)
		}
		seenNodeURLs[n.URL] = true
		if c.Nodes[i].Runtime == "" {
			c.Nodes[i].Runtime = "ollama"
		}
		switch c.Nodes[i].Runtime {
		case "ollama", "vllm", "tgi", "llamacpp":
			// valid
		default:
			return fmt.Errorf("node %s: unknown runtime %q (valid: ollama, vllm, tgi, llamacpp)", n.Name, c.Nodes[i].Runtime)
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

	if c.Audit.Path == "" {
		c.Audit.Path = "audit.log"
	}

	if c.Savings.ReferenceCostPer1K <= 0 {
		c.Savings.ReferenceCostPer1K = 0.002
	}

	if c.HA.HeartbeatIntervalMs <= 0 {
		c.HA.HeartbeatIntervalMs = 5000
	}
	if c.HA.PeerTimeoutMs <= 0 {
		c.HA.PeerTimeoutMs = 3000
	}
	for i, peer := range c.HA.Peers {
		u, err := url.Parse(peer)
		if err != nil {
			return fmt.Errorf("ha peer %d: invalid URL %q: %w", i, peer, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("ha peer %d URL must be http(s) with a host: %s", i, peer)
		}
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
