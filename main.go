package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/bench"
	"github.com/ollama-mesh/ollama-mesh/internal/cli"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
	"github.com/ollama-mesh/ollama-mesh/internal/proxy"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
	"github.com/ollama-mesh/ollama-mesh/internal/winexit"
)

// Version is set at build time via ldflags: -X main.Version=v0.x.y
// Defaults to "dev" for local/untagged builds.
var Version = "dev"

// stringSliceFlag implements flag.Value for a repeatable string flag
// (the standard library's flag package has no built-in for this).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ", ") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// printStartupBanner prints a one-time onboarding summary when the database
// has no nodes or API keys yet - there is no config.yaml to point at
// anymore, so the dashboard is the only setup path.
func printStartupBanner(cfg *config.Config, dbPath string) {
	line := "================================================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  ollama-mesh - blank slate (no nodes or API keys configured yet)")
	fmt.Println(line)
	fmt.Println()
	fmt.Printf("  Database:            %s\n", dbPath)
	fmt.Printf("  Point your apps at:  http://localhost:%d\n", cfg.Proxy.Port)
	fmt.Println()
	fmt.Println("  Dashboard:           http://localhost:8080")
	fmt.Println("  Dashboard login:     admin / admin (you'll be asked to set a new password on first login)")
	fmt.Println()
	fmt.Println("  Add your first GPU node and API key from the dashboard - or run")
	fmt.Println("  install.sh's network probe to discover and add them automatically.")
	fmt.Println(line)
	fmt.Println()
}

// seedNodesToStore parses repeatable --seed-node "name=...,url=...,runtime=..."
// values and writes them directly into the database, then exits without
// starting any servers. Used by install.sh's interactive runtime-probe
// wizard so shell code never needs to know the SQLite schema.
func seedNodesToStore(dbPath string, specs []string) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	for _, spec := range specs {
		fields := map[string]string{}
		for _, part := range strings.Split(spec, ",") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				return fmt.Errorf("invalid --seed-node field %q (want key=value)", part)
			}
			fields[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
		name, url := fields["name"], fields["url"]
		if name == "" || url == "" {
			return fmt.Errorf("--seed-node %q: name and url are required", spec)
		}
		runtime := fields["runtime"]
		if runtime == "" {
			runtime = "ollama"
		}
		switch runtime {
		case "ollama", "vllm", "tgi", "llamacpp", "mlx":
		default:
			return fmt.Errorf("--seed-node %q: unknown runtime %q (valid: ollama, vllm, tgi, llamacpp, mlx)", spec, runtime)
		}
		if err := st.UpsertNode(store.NodeRecord{Name: name, URL: url, Runtime: runtime}); err != nil {
			return fmt.Errorf("seed node %q: %w", name, err)
		}
		fmt.Printf("Added node %q (%s, runtime=%s)\n", name, url, runtime)
	}
	return nil
}

// applyPersistedSettings overlays every SQLite-backed setting onto cfg before
// the servers start. This is the sole configuration path now that
// config.yaml is gone (2026-07 elimination) - cfg starts from Validate()'s
// built-in defaults, and every field an operator can change through the
// dashboard's Settings page has a corresponding settings key here.
func applyPersistedSettings(cfg *config.Config, st store.Store) {
	cfg.Timezone = store.GetStringSetting(st, "timezone", cfg.Timezone)

	cfg.Proxy.Port = store.GetIntSetting(st, "proxy_port", cfg.Proxy.Port)
	cfg.Proxy.LogFormat = store.GetStringSetting(st, "proxy_log_format", cfg.Proxy.LogFormat)
	accessLog := store.GetBoolSetting(st, "proxy_access_log", cfg.Proxy.AccessLog == nil || *cfg.Proxy.AccessLog)
	cfg.Proxy.AccessLog = &accessLog
	cfg.Proxy.TrustProxyHeaders = store.GetBoolSetting(st, "proxy_trust_proxy_headers", cfg.Proxy.TrustProxyHeaders)

	cfg.Admin.BindAddress = store.GetStringSetting(st, "admin_bind_address", cfg.Admin.BindAddress)
	cfg.Admin.CORSOrigin = store.GetStringSetting(st, "admin_cors_origin", cfg.Admin.CORSOrigin)

	if v := store.GetStringSetting(st, "routing_strategy", ""); v != "" {
		cfg.Routing.Strategy = v
	}
	cfg.Routing.Fallback = store.GetStringSetting(st, "routing_fallback", cfg.Routing.Fallback)
	cfg.Routing.UpstreamTimeoutMs = store.GetIntSetting(st, "routing_upstream_timeout_ms", cfg.Routing.UpstreamTimeoutMs)
	cfg.Routing.MaxRetries = store.GetIntSetting(st, "routing_max_retries", cfg.Routing.MaxRetries)
	cfg.Routing.AllowManagementEndpoints = store.GetBoolSetting(st, "routing_allow_management_endpoints", cfg.Routing.AllowManagementEndpoints)
	cfg.Routing.SessionAffinity = store.GetBoolSetting(st, "routing_session_affinity", cfg.Routing.SessionAffinity)
	cfg.Routing.SessionAffinityTTL = store.GetStringSetting(st, "routing_session_affinity_ttl", cfg.Routing.SessionAffinityTTL)
	cfg.Routing.NvidiaPollIntervalMs = store.GetIntSetting(st, "routing_nvidia_poll_interval_ms", cfg.Routing.NvidiaPollIntervalMs)
	cfg.Routing.QueueMaxDepth = store.GetIntSetting(st, "routing_queue_max_depth", cfg.Routing.QueueMaxDepth)
	cfg.Routing.QueueTimeoutMs = store.GetIntSetting(st, "routing_queue_timeout_ms", cfg.Routing.QueueTimeoutMs)
	cfg.Routing.HealthFailureThreshold = store.GetIntSetting(st, "routing_health_failure_threshold", cfg.Routing.HealthFailureThreshold)
	cfg.Routing.HealthSuccessThreshold = store.GetIntSetting(st, "routing_health_success_threshold", cfg.Routing.HealthSuccessThreshold)
	cfg.Routing.OverflowSLAMs = store.GetIntSetting(st, "routing_overflow_sla_ms", cfg.Routing.OverflowSLAMs)
	cfg.Routing.MaxInFlightPerNode = store.GetIntSetting(st, "routing_max_in_flight_per_node", cfg.Routing.MaxInFlightPerNode)
	cfg.Routing.ThermalWatchdog.Enabled = store.GetBoolSetting(st, "routing_thermal_watchdog_enabled", cfg.Routing.ThermalWatchdog.Enabled)
	cfg.Routing.ThermalWatchdog.MaxTempCelsius = store.GetFloatSetting(st, "routing_thermal_watchdog_max_temp_celsius", cfg.Routing.ThermalWatchdog.MaxTempCelsius)
	cfg.Routing.ThermalWatchdog.ConsecutiveBreaches = store.GetIntSetting(st, "routing_thermal_watchdog_consecutive_breaches", cfg.Routing.ThermalWatchdog.ConsecutiveBreaches)
	store.GetJSONSetting(st, "routing_fallback_chains", &cfg.Routing.FallbackChains)
	store.GetJSONSetting(st, "routing_local_degradation_chains", &cfg.Routing.LocalDegradationChains)

	cfg.Metrics.Enabled = store.GetBoolSetting(st, "metrics_enabled", cfg.Metrics.Enabled)
	cfg.Metrics.Port = store.GetIntSetting(st, "metrics_port", cfg.Metrics.Port)

	cfg.LiteLLM.Enabled = store.GetBoolSetting(st, "litellm_enabled", cfg.LiteLLM.Enabled)
	cfg.LiteLLM.URL = store.GetStringSetting(st, "litellm_url", cfg.LiteLLM.URL)
	cfg.LiteLLM.APIKey = store.GetStringSetting(st, "litellm_api_key", cfg.LiteLLM.APIKey)

	cfg.Docker.Enabled = store.GetBoolSetting(st, "docker_enabled", cfg.Docker.Enabled)
	cfg.Docker.Socket = store.GetStringSetting(st, "docker_socket", cfg.Docker.Socket)
	cfg.Docker.PollIntervalMs = store.GetIntSetting(st, "docker_poll_interval_ms", cfg.Docker.PollIntervalMs)

	// Defaults to true (fallback applies only on first boot, before this key
	// is ever written) - per-request audit logging is the data source for the
	// Request Log page, so a fresh install must be able to show requests
	// without the operator first discovering and flipping a settings toggle.
	cfg.Audit.Enabled = store.GetBoolSetting(st, "audit_enabled", true)
	// 30-day fallback applies only if this key was never set (first boot) -
	// GetIntSetting returns a stored "0" as 0 (indefinite), not the fallback.
	cfg.Audit.RetentionDays = store.GetIntSetting(st, "audit_retention_days", 30)
	// System audit (admin action trail) defaults to forever - low-volume,
	// security-sensitive, and 0 is both the fallback and the zero value.
	cfg.Audit.SystemAuditRetentionDays = store.GetIntSetting(st, "audit_system_retention_days", 0)

	cfg.Webhook.Enabled = store.GetBoolSetting(st, "webhook_enabled", cfg.Webhook.Enabled)
	cfg.Webhook.URL = store.GetStringSetting(st, "webhook_url", cfg.Webhook.URL)
	cfg.Webhook.Secret = store.GetStringSetting(st, "webhook_secret", cfg.Webhook.Secret)

	cfg.Savings.ReferenceCostPer1K = store.GetFloatSetting(st, "savings_reference_cost_per_1k", cfg.Savings.ReferenceCostPer1K)

	cfg.Warmup.Enabled = store.GetBoolSetting(st, "warmup_enabled", cfg.Warmup.Enabled)
	cfg.Warmup.IntervalMs = store.GetIntSetting(st, "warmup_interval_ms", cfg.Warmup.IntervalMs)
	cfg.Warmup.KeepAlive = store.GetStringSetting(st, "warmup_keep_alive", cfg.Warmup.KeepAlive)
	store.GetJSONSetting(st, "warmup_models", &cfg.Warmup.Models)

	cfg.CloudBudget.DailyUSDCap = store.GetFloatSetting(st, "cloud_daily_usd_cap", cfg.CloudBudget.DailyUSDCap)
	cfg.CloudBudget.MonthlyUSDCap = store.GetFloatSetting(st, "cloud_monthly_usd_cap", cfg.CloudBudget.MonthlyUSDCap)

	cfg.HuggingFace.Token = store.GetStringSetting(st, "huggingface_token", cfg.HuggingFace.Token)

	cfg.Backup.Enabled = store.GetBoolSetting(st, "backup_enabled", cfg.Backup.Enabled)
	cfg.Backup.IntervalHours = store.GetIntSetting(st, "backup_interval_hours", cfg.Backup.IntervalHours)
	cfg.Backup.RetentionCount = store.GetIntSetting(st, "backup_retention_count", cfg.Backup.RetentionCount)
	cfg.Backup.TargetDir = store.GetStringSetting(st, "backup_target_dir", cfg.Backup.TargetDir)

	store.GetJSONSetting(st, "context_windows", &cfg.ContextWindows)
}

// resolveCommand decides which path main() should take for the given raw
// os.Args[1:], kept as a pure function (no I/O, no globals) so dispatch
// logic is unit-testable without spinning up servers. Dash-prefixed tokens
// (e.g. "-version") never match a bare subcommand word, so root's own flags
// and the CLI's subcommand names never collide.
func resolveCommand(args []string) string {
	if len(args) == 0 {
		return "server"
	}
	switch args[0] {
	case "help", "-h", "--help":
		return "help"
	case "bench":
		return "bench"
	case "agent":
		return "agent"
	case "version", "status", "login", "logout", "whoami", "nodes", "models", "runtime", "node":
		return "cli"
	default:
		return "server"
	}
}

// printTopLevelHelp is the single, unified --help output for the merged
// binary - one command table covering the server, the Node Agent, the
// benchmark tool, and the Admin API CLI, rather than separate help systems
// per subcommand family.
// helpTableRows renders as a two-column, tab-aligned list via
// text/tabwriter rather than a hand-spaced string literal - alignment is
// then correct regardless of any row's length, instead of silently drifting
// out of alignment the moment one row's length changes (as happened here
// before this fix).
var helpTableRows = [][2]string{
	{"ollama-mesh [flags]", "run the mesh server (default)"},
	{"ollama-mesh agent [flags]", "run the Node Agent (node-local execution point for the mesh)"},
	{"ollama-mesh bench [flags]", "warm-vs-cold first-token latency benchmark"},
	{"ollama-mesh version", "print version"},
	{"ollama-mesh status", "print mesh health/status summary"},
	{"ollama-mesh login", "authenticate once and save the session locally (recommended)"},
	{"ollama-mesh logout", "remove the saved session"},
	{"ollama-mesh whoami", "show the CLI's saved identity (live-verified)"},
	{"ollama-mesh nodes", "list nodes known to the mesh"},
	{"ollama-mesh models [action] ...", "fleet-wide list, or pull/delete/unload/list on one node"},
	{"ollama-mesh runtime <action> ...", "start/stop/restart/logs/drain/undrain/health on one node"},
	{"ollama-mesh node control ...", "node enrollment probe/accept"},
}

func printTopLevelHelp() {
	fmt.Fprintf(os.Stderr, "ollama-mesh %s - the self-hosted control plane for AI inference: warm-aware GPU routing, an OpenAI-compatible gateway, and cost-metered cloud overflow for Ollama, vLLM, TGI, llama.cpp, and MLX\n\nUsage:\n", Version)
	tw := tabwriter.NewWriter(os.Stderr, 0, 4, 2, ' ', 0)
	for _, r := range helpTableRows {
		fmt.Fprintf(tw, "  %s\t%s\n", r[0], r[1])
	}
	tw.Flush()
	fmt.Fprint(os.Stderr, `
Run "ollama-mesh <command> --help" for the full list of actions and flags for
that command.

Server flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, "\nNo config file needed: start the binary, then add nodes/API keys/settings\nthrough the dashboard at http://localhost:8080 (admin/admin on first run).\n")
}

func main() {
	var (
		showVersion   = flag.Bool("version", false, "print version and exit")
		dbFlag        = flag.String("db", "", "path to the SQLite database (overrides MESH_DB_PATH env; default mesh.db)")
		logFormatFlag = flag.String("log-format", "", "log output format: text (default) or json")
		seedNodes     stringSliceFlag
	)
	flag.Var(&seedNodes, "seed-node", `add a node directly to the database and exit, format: "name=...,url=...,runtime=..." (repeatable)`)
	flag.Usage = printTopLevelHelp

	// Subcommand dispatch: check before flag.Parse() so each subcommand
	// owns its own flag set and does not pollute the main flag namespace.
	switch resolveCommand(os.Args[1:]) {
	case "help":
		printTopLevelHelp()
		return
	case "bench":
		bench.Run(os.Args[2:])
		return
	case "agent":
		nodeagent.Run(os.Args[2:], Version)
		return
	case "cli":
		cli.Version = Version
		os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
	}

	flag.Parse()

	if *showVersion {
		fmt.Printf("ollama-mesh %s\n", Version)
		return
	}

	dbPath := *dbFlag
	if dbPath == "" {
		dbPath = os.Getenv("MESH_DB_PATH")
	}
	if dbPath == "" {
		dbPath = "mesh.db"
	}

	// MESH_BACKUP_DIR seeds the default scheduled-backup target directory
	// (config.BackupConfig.TargetDir), same env-var pattern as MESH_DB_PATH
	// above. docker-compose.yml sets this to /backups - a distinct named
	// volume from mesh-data's /data mount - so a scheduled backup survives a
	// docker volume rm/down -v on the data volume, and vice versa. The
	// bare-metal fallback keeps backups next to the database when no data
	// volume separation is possible anyway.
	backupDir := os.Getenv("MESH_BACKUP_DIR")
	if backupDir == "" && dbPath != "-" {
		backupDir = filepath.Join(filepath.Dir(dbPath), "backups")
	}

	if len(seedNodes) > 0 {
		if err := seedNodesToStore(dbPath, seedNodes); err != nil {
			winexit.Fatalf("seed nodes: %v", err)
		}
		return
	}

	cfg := &config.Config{}
	cfg.Backup.TargetDir = backupDir
	if err := cfg.Validate(); err != nil {
		winexit.Fatalf("invalid default config: %v", err)
	}

	// Open the SQLite persistence store. "-" disables it (NopStore) for
	// advanced/ephemeral/test use only - since SQLite is now the sole
	// configuration store, nothing added via the dashboard survives a
	// restart in that mode.
	var st store.Store = store.NopStore{}
	if dbPath != "-" {
		var stErr error
		st, stErr = store.Open(dbPath)
		if stErr != nil {
			winexit.Fatalf("failed to open store: %v", stErr)
		}
		defer st.Close()
	}

	// Apply every persisted setting before anything below reads cfg, since
	// this is now the only configuration source (no config.yaml).
	applyPersistedSettings(cfg, st)

	// Configure structured logging. CLI flag takes precedence over the
	// persisted setting.
	logFormat := cfg.Proxy.LogFormat
	if *logFormatFlag != "" {
		logFormat = *logFormatFlag
	}
	if logFormat == "json" {
		h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
		slog.SetDefault(slog.New(h))
		// Redirect legacy log.Printf calls (from internal packages) through slog
		// so all output is machine-parseable JSON when log_format=json is set.
		log.SetFlags(0)
		log.SetOutput(slog.NewLogLogger(h, slog.LevelInfo).Writer())
	}

	log.Printf("ollama-mesh %s starting...", Version)
	log.Printf("Database        : %s", dbPath)
	log.Printf("Proxy port      : %d", cfg.Proxy.Port)
	log.Printf("Auth enabled    : %t", cfg.Auth.IsEnabled())
	log.Printf("Metrics port    : %d", cfg.Metrics.Port)
	log.Printf("Poll interval   : %dms", cfg.Routing.PollIntervalMs)

	authMw := auth.NewMiddleware(cfg.Auth)

	// Per-key usage/quota counters persist across restarts via SQLite.
	if err := authMw.LoadFromStore(st); err != nil {
		log.Printf("WARNING: could not restore key counters from store: %v", err)
	}

	// All API keys live in the store now - no config.yaml keys to merge.
	keyCount := 0
	if runtimeKeys, err := st.AllKeys(); err == nil {
		for _, k := range runtimeKeys {
			if k.Revoked {
				authMw.RevokeKey(k.Name)
				continue
			}
			authMw.AddKey(config.KeyConfig{
				Name:         k.Name,
				Key:          k.Key,
				RateLimit:    k.RateLimit,
				DailyLimit:   k.DailyLimit,
				MonthlyLimit: k.MonthlyLimit,
				Models:       k.Models,
				ExpiresAt:    k.ExpiresAt,
			})
			keyCount++
		}
		if keyCount > 0 {
			log.Printf("store: loaded %d API key(s)", keyCount)
		}
	} else {
		log.Printf("WARNING: could not load API keys from store: %v", err)
	}

	// Loud, unmissable warning when the proxy is running without authentication.
	if !cfg.Auth.IsEnabled() {
		log.Printf("WARNING: ================================================================")
		log.Printf("WARNING: AUTHENTICATION IS DISABLED - every request is forwarded with no")
		log.Printf("WARNING: API key check. Anyone who can reach :%d has full access to your", cfg.Proxy.Port)
		log.Printf("WARNING: backend models. Enable auth and add at least one key from the")
		log.Printf("WARNING: dashboard for any non-loopback or shared deployment.")
		log.Printf("WARNING: ================================================================")
	}

	r := router.New(cfg.Routing, cfg.Nodes, cfg.CloudProviders)
	r.SetTimezone(cfg.Timezone)
	r.SetLiteLLM(cfg.LiteLLM)
	r.SetStore(st)
	r.SetDockerConfig(cfg.Docker)
	if cfg.Docker.Enabled {
		log.Printf("Docker auto-discovery enabled (socket: %s)", cfg.Docker.Socket)
	}
	r.SetWarmupConfig(cfg.Warmup)
	r.SetWebhookConfig(cfg.Webhook)
	if cfg.Warmup.Enabled && len(cfg.Warmup.Models) > 0 {
		log.Printf("Model warmup enabled: %d model(s), interval %dms, keep_alive %s",
			len(cfg.Warmup.Models), cfg.Warmup.IntervalMs, cfg.Warmup.KeepAlive)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Start(ctx)

	// Nodes: entirely store-driven now (added via the dashboard or
	// install.sh's --seed-node wizard).
	nodeCount := 0
	if runtimeNodes, err := st.AllNodes(); err == nil {
		for _, n := range runtimeNodes {
			vram := int64(0)
			if n.VRAMTotalMB != nil {
				vram = *n.VRAMTotalMB
			}
			rt := n.Runtime
			if rt == "" {
				rt = "ollama"
			}
			r.AddNode(config.NodeConfig{
				Name:        n.Name,
				URL:         n.URL,
				Host:        n.Host,
				Runtime:     rt,
				VRAMTotalMB: vram,
			})
			nodeCount++
		}
		if nodeCount > 0 {
			log.Printf("store: loaded %d node(s)", nodeCount)
		}
	} else {
		log.Printf("WARNING: could not load nodes from store: %v", err)
	}
	if overrides, err := st.NodeOverrides(); err == nil {
		for name, ov := range overrides {
			r.PatchNode(name, router.NodePatch{VRAMTotalMB: ov.VRAMTotalMB, GPUModel: ov.GPUModel, Runtime: ov.Runtime, GPUIndices: ov.GPUIndices, MaxInFlight: ov.MaxInFlight, TLSFingerprint: ov.TLSFingerprint})
		}
	}
	if drains, err := st.NodeDrainStates(); err == nil {
		for name, ds := range drains {
			if ds.Draining {
				r.DrainNode(name, ds.Reason)
			}
		}
	}
	if agents, err := st.AllNodeAgents(); err == nil {
		for _, a := range agents {
			if !a.Enabled {
				continue
			}
			// a.Name was the persisted key before this change - historically
			// always a node name (node_agent was per-node, keyed 1:1 with
			// runtime_nodes.name). Now that nodeAgents is host-keyed,
			// translate: if a.Name still matches an existing node, resolve
			// that node's Host and use it as the key, so an upgrade doesn't
			// silently orphan every pre-existing agent-enabled install
			// (Host defaults to the URL's hostname, not the node name, so
			// the two rarely coincide). If no node matches a.Name, it's
			// either already a host string (a fresh install created after
			// this change) or a stale orphaned record either way - use it
			// as-is; it simply sits unused until a node with that host
			// exists.
			key := a.Name
			if host, ok := r.NodeHost(a.Name); ok {
				key = host
			}
			r.SetNodeAgent(key, true, a.Port, a.Token)
		}
	} else {
		log.Printf("WARNING: could not load node agents from store: %v", err)
	}
	if controls, err := st.AllNodeControl(); err == nil {
		for _, c := range controls {
			if c.Configured {
				r.SetNodeControl(c.Name, router.ControlConfig{Driver: c.Driver, Identifier: c.Identifier, Configured: true, StartCommand: c.StartCommand})
			}
		}
	} else {
		log.Printf("WARNING: could not load node control config from store: %v", err)
	}

	// Cloud providers: entirely store-driven now.
	if providers, err := st.AllCloudProviders(); err == nil {
		clouds := make([]config.CloudProvider, len(providers))
		for i, p := range providers {
			clouds[i] = config.CloudProvider{
				Name:            p.Name,
				Provider:        p.Provider,
				BaseURL:         p.BaseURL,
				APIKey:          p.APIKey,
				DefaultModel:    p.DefaultModel,
				CostPer1KTokens: p.CostPer1KTokens,
				Enabled:         p.Enabled,
				Priority:        p.Priority,
			}
		}
		r.SetClouds(clouds)
		cfg.CloudProviders = clouds
		if len(clouds) > 0 {
			log.Printf("store: loaded %d cloud provider(s)", len(clouds))
		}
	} else {
		log.Printf("WARNING: could not load cloud providers from store: %v", err)
	}

	// Load persisted predictive transition history so the predictive engine
	// resumes learned patterns instead of rebuilding from zero.
	if hist, err := st.PredictiveHistory(); err == nil && len(hist) > 0 {
		entries := make([]router.TransitionEntry, len(hist))
		for i, h := range hist {
			entries[i] = router.TransitionEntry{
				FromModel: h.FromModel,
				ToModel:   h.ToModel,
				Timestamp: h.Timestamp,
				HourOfDay: h.Timestamp.Hour(),
			}
		}
		r.SeedPredictiveHistory(entries)
		log.Printf("store: loaded %d predictive transition(s)", len(entries))
	}

	// Restore the warm-state residency map so the router starts warm, not cold:
	// LRU last-used history is re-seeded for every persisted (model, node) pair,
	// and each node's residency is seeded until the first live poll refreshes it.
	// Runs after all nodes are registered and before the proxy serves traffic.
	if n, err := r.RestoreWarmState(); err != nil {
		log.Printf("WARNING: could not restore warm state from store: %v", err)
	} else if n > 0 {
		log.Printf("store: restored warm state for %d (model,node) pair(s)", n)
	}

	// Restore sticky-session affinity so a restart doesn't drop every
	// in-flight session and force a cold KV-cache round-trip on its next
	// request (.local/audit-fixes-2026-08-03.md #7). Still only a soft
	// preference at restore time - Route re-validates health/draining
	// before honoring any restored entry.
	if n, err := r.RestoreAffinity(); err != nil {
		log.Printf("WARNING: could not restore session affinity from store: %v", err)
	} else if n > 0 {
		log.Printf("store: restored session affinity for %d session(s)", n)
	}

	// Load persisted per-node warmup settings (admin-toggled) from the KV store
	// so proactive warmup survives a restart.
	if settings, err := st.AllSettings(); err == nil {
		loaded := 0
		for k, v := range settings {
			if pn, ok := strings.CutPrefix(k, "pinned:node:"); ok {
				var models []string
				if v != "" && json.Unmarshal([]byte(v), &models) == nil && len(models) > 0 {
					r.SetPinnedModels(pn, models)
				}
				continue
			}
			name, ok := strings.CutPrefix(k, "warmup:node:")
			if !ok || v == "" {
				continue
			}
			var nw router.NodeWarmup
			if json.Unmarshal([]byte(v), &nw) == nil && (nw.Enabled || len(nw.Models) > 0) {
				r.SetNodeWarmup(name, nw.Enabled, nw.Models)
				loaded++
			}
		}
		if loaded > 0 {
			log.Printf("store: loaded warmup settings for %d node(s)", loaded)
		}
	}

	// Load persisted schedules (warmup/drain/undrain) from the KV store.
	if raw, err := st.GetSetting("schedules"); err == nil && raw != "" {
		var scheds []router.Schedule
		if json.Unmarshal([]byte(raw), &scheds) == nil && len(scheds) > 0 {
			r.SetSchedules(scheds)
			log.Printf("store: loaded %d schedule(s)", len(scheds))
		}
	}

	// Load persisted routing rules.
	if rules, err := st.AllRoutingRules(); err == nil {
		for _, rule := range rules {
			r.AddRule(config.RoutingRule{
				ID:         rule.ID,
				Condition:  rule.Condition,
				TargetNode: rule.Target,
				Priority:   rule.Priority,
				Enabled:    rule.Enabled,
			})
		}
		if len(rules) > 0 {
			log.Printf("store: loaded %d routing rule(s)", len(rules))
		}
	} else {
		log.Printf("WARNING: could not load routing rules from store: %v", err)
	}

	if nodeCount == 0 && keyCount == 0 {
		printStartupBanner(cfg, dbPath)
	}

	// Periodically flush usage counters so a restart preserves quota/usage
	// state (crash loses at most one interval).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := authMw.SaveToStore(st); err != nil {
					log.Printf("WARNING: usage state flush failed: %v", err)
				}
			}
		}
	}()

	auditLog := audit.New(st, cfg.Audit.Enabled)
	defer auditLog.Close()

	adminSrv := admin.NewServer(r, authMw, *cfg, st)
	adminSrv.SetAuditLogger(auditLog)
	adminSrv.SetVersion(Version)
	if err := adminSrv.LoadFromStore(); err != nil {
		log.Printf("WARNING: could not restore analytics from store: %v", err)
	}
	adminSrv.StartCounterFlush(ctx)
	adminSrv.StartPeriodicCleanup(ctx)
	adminSrv.StartBackupScheduler(ctx)

	// One-click restore (P49 follow-up): admin.go validates a restore
	// request and, if valid, sends the full path of the chosen backup file
	// down this channel - the actual database swap and process exit only
	// ever happen here in main(), which already owns graceful shutdown and
	// knows dbPath. Buffered 1 so a restore request never blocks on the main
	// select loop; a second concurrent request while one is pending is
	// rejected by admin.go's non-blocking send (see handleRestoreBackup).
	restoreCh := make(chan string, 1)
	adminSrv.SetRestoreChannel(restoreCh)

	proxyHandler := proxy.NewHandler(r, adminSrv, auditLog)
	proxyHandler.SetAuth(authMw)
	proxyHandler.SetAllowManagementEndpoints(cfg.Routing.AllowManagementEndpoints)
	proxyHandler.SetTrustProxyHeaders(cfg.Proxy.TrustProxyHeaders)
	adminSrv.SetProxyHandler(proxyHandler)
	accessEnabled := cfg.Proxy.AccessLog == nil || *cfg.Proxy.AccessLog
	proxyHandler.SetAccessLogger(proxy.NewAccessLogger(os.Stdout, accessEnabled))
	log.Printf("Access log     : %t (stdout, JSON lines)", accessEnabled)
	// RecoverMiddleware is the outermost wrapper so a handler panic returns a
	// clean 500 and increments a metric instead of an ugly connection drop.
	wrapped := proxy.RecoverMiddleware(proxy.SecurityHeaders(authMw.Handler(proxyHandler)))

	proxySrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Proxy.Port),
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	var metricsSrv *http.Server
	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsSrv = &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.Metrics.Port),
			Handler:           proxy.SecurityHeaders(mux),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			log.Printf("Metrics server listening on :%d/metrics", cfg.Metrics.Port)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Metrics server error: %v", err)
			}
		}()
	}

	adminHttpSrv := &http.Server{
		Addr:              cfg.Admin.BindAddress,
		Handler:           proxy.RecoverMiddleware(proxy.SecurityHeaders(adminSrv.Handler())),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		log.Printf("Admin dashboard listening on %s", cfg.Admin.BindAddress)
		if err := adminHttpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Admin server error: %v", err)
		}
	}()

	go func() {
		log.Printf("Proxy listening on :%d", cfg.Proxy.Port)
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Proxy server error: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	reloadCh := make(chan os.Signal, 1)
	setupReloadSignal(reloadCh)

	var pendingRestorePath string
	for {
		select {
		case <-reloadCh:
			added, removed, authKeys, cloudProviders, err := adminSrv.ReloadFromStore()
			if err != nil {
				log.Printf("reload from store failed: %v (keeping previous state)", err)
				continue
			}
			log.Printf("reloaded from store (auth keys: %d, nodes: +%d/-%d, cloud providers: %d)",
				authKeys, added, removed, cloudProviders)
			continue
		case pendingRestorePath = <-restoreCh:
		case <-sig:
		}
		break
	}
	if pendingRestorePath != "" {
		log.Printf("Restore requested: shutting down to swap mesh.db for %s", pendingRestorePath)
	} else {
		log.Println("Shutting down gracefully...")
	}

	// Derived from proxySrv's own WriteTimeout (not a bare literal) so a
	// SIGINT/SIGTERM during an active long-running stream isn't cut off
	// before the request could have finished on its own.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), proxySrv.WriteTimeout+5*time.Second)
	defer shutdownCancel()

	if err := proxySrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Proxy shutdown error: %v", err)
	}
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Metrics shutdown error: %v", err)
		}
	}
	if err := adminHttpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Admin shutdown error: %v", err)
	}
	cancel()

	// Drain the async request-log queue before the store closes (deferred
	// above), so the logger goroutine can't write through a closed store.
	adminSrv.Shutdown()

	// Final flush so the just-served requests are not lost on restart.
	if err := authMw.SaveToStore(st); err != nil {
		log.Printf("WARNING: final usage state flush failed: %v", err)
	}
	// Tier 3: flush the full warm-state residency snapshot on graceful shutdown so
	// the router restores its warm set on the next start.
	r.FlushWarmState()
	// Same tier for sticky-session affinity, so a graceful restart doesn't
	// drop in-flight sessions either (.local/audit-fixes-2026-08-03.md #7).
	r.FlushAffinity()

	if pendingRestorePath != "" {
		// os.Exit inside performRestore skips every defer below main() -
		// st.Close() (deferred near store.Open above) never fires on this
		// path, so close what still needs closing explicitly before calling it.
		auditLog.Close()
		performRestore(dbPath, pendingRestorePath, st)
		// performRestore always calls os.Exit itself; never returns.
	}

	log.Println("Shutdown complete")
}

// performRestore swaps dbPath for the contents of backupPath (already
// validated by admin.go's handleRestoreBackup before this was ever reached)
// and exits the process non-zero so the deployment's process supervisor
// (systemd Restart=on-failure, Docker restart:unless-stopped, Kubernetes'
// default restartPolicy) brings the mesh back up with the restored database.
// Always calls os.Exit - a bare-metal run with no supervisor configured will
// simply stay down until started manually; docs/backup.md documents that
// caveat explicitly rather than leaving it a silent surprise.
func performRestore(dbPath, backupPath string, st store.Store) {
	if err := st.Close(); err != nil {
		log.Printf("WARNING: error closing store before restore: %v", err)
	}

	// Stage the full copy in a temp file alongside dbPath first, so a
	// mid-copy failure never leaves the live mesh.db truncated or corrupt -
	// the live file is only ever touched by the final atomic rename below,
	// once the replacement is proven complete.
	tmpPath := dbPath + ".restoring"
	src, err := os.Open(backupPath)
	if err != nil {
		log.Printf("ERROR: restore aborted - could not open backup file %s: %v", backupPath, err)
		log.Println("ERROR: mesh.db was NOT modified - restart the mesh manually; it resumes with the existing database")
		winexit.Exit(1)
	}
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		src.Close()
		log.Printf("ERROR: restore aborted - could not create %s: %v", tmpPath, err)
		log.Println("ERROR: mesh.db was NOT modified - restart the mesh manually; it resumes with the existing database")
		winexit.Exit(1)
	}
	_, copyErr := io.Copy(dst, src)
	src.Close()
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		log.Printf("ERROR: restore aborted mid-copy (copy error: %v, close error: %v)", copyErr, closeErr)
		log.Println("ERROR: mesh.db was NOT modified - restart the mesh manually; it resumes with the existing database")
		winexit.Exit(1)
	}

	// Re-validate the staged copy itself, not just backupPath earlier in
	// admin.go: the graceful-shutdown drain above can take up to
	// proxySrv.WriteTimeout+5s between that validation and this copy, during
	// which the source file could change underneath it (e.g. a concurrent
	// scheduled backup/retention prune, or filesystem trouble). This is the
	// last chance to catch corruption before the swap makes tmpPath the live
	// database - never fabricate a "restore complete" on unverified bytes.
	if err := store.ValidateBackupFile(tmpPath); err != nil {
		os.Remove(tmpPath)
		log.Printf("ERROR: restore aborted - staged copy failed validation: %v", err)
		log.Println("ERROR: mesh.db was NOT modified - restart the mesh manually; it resumes with the existing database")
		winexit.Exit(1)
	}

	// WAL/SHM sidecars belong to the OLD database contents - remove them so
	// the restored file isn't reconciled against stale write-ahead data on
	// next boot.
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	if err := os.Rename(tmpPath, dbPath); err != nil {
		log.Printf("ERROR: restore failed at the final swap: %v", err)
		log.Printf("ERROR: the validated replacement is staged at %s - move it to %s manually, then restart", tmpPath, dbPath)
		winexit.Exit(1)
	}

	log.Printf("Restore complete: %s -> %s", backupPath, dbPath)
	log.Println("Exiting so the process supervisor restarts the mesh with the restored database")
	winexit.Exit(1)
}
