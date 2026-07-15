package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/bench"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/ha"
	"github.com/ollama-mesh/ollama-mesh/internal/proxy"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
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
		case "ollama", "vllm", "tgi", "llamacpp":
		default:
			return fmt.Errorf("--seed-node %q: unknown runtime %q (valid: ollama, vllm, tgi, llamacpp)", spec, runtime)
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
	cfg.Routing.ThermalWatchdog.Enabled = store.GetBoolSetting(st, "routing_thermal_watchdog_enabled", cfg.Routing.ThermalWatchdog.Enabled)
	cfg.Routing.ThermalWatchdog.MaxTempCelsius = store.GetFloatSetting(st, "routing_thermal_watchdog_max_temp_celsius", cfg.Routing.ThermalWatchdog.MaxTempCelsius)
	cfg.Routing.ThermalWatchdog.ConsecutiveBreaches = store.GetIntSetting(st, "routing_thermal_watchdog_consecutive_breaches", cfg.Routing.ThermalWatchdog.ConsecutiveBreaches)
	store.GetJSONSetting(st, "routing_fallback_chains", &cfg.Routing.FallbackChains)

	cfg.Metrics.Enabled = store.GetBoolSetting(st, "metrics_enabled", cfg.Metrics.Enabled)
	cfg.Metrics.Port = store.GetIntSetting(st, "metrics_port", cfg.Metrics.Port)

	cfg.LiteLLM.Enabled = store.GetBoolSetting(st, "litellm_enabled", cfg.LiteLLM.Enabled)
	cfg.LiteLLM.URL = store.GetStringSetting(st, "litellm_url", cfg.LiteLLM.URL)

	cfg.Docker.Enabled = store.GetBoolSetting(st, "docker_enabled", cfg.Docker.Enabled)
	cfg.Docker.Socket = store.GetStringSetting(st, "docker_socket", cfg.Docker.Socket)
	cfg.Docker.PollIntervalMs = store.GetIntSetting(st, "docker_poll_interval_ms", cfg.Docker.PollIntervalMs)

	cfg.Audit.Enabled = store.GetBoolSetting(st, "audit_enabled", cfg.Audit.Enabled)

	cfg.Webhook.Enabled = store.GetBoolSetting(st, "webhook_enabled", cfg.Webhook.Enabled)
	cfg.Webhook.URL = store.GetStringSetting(st, "webhook_url", cfg.Webhook.URL)
	cfg.Webhook.Secret = store.GetStringSetting(st, "webhook_secret", cfg.Webhook.Secret)

	cfg.Savings.ReferenceCostPer1K = store.GetFloatSetting(st, "savings_reference_cost_per_1k", cfg.Savings.ReferenceCostPer1K)

	cfg.HA.Enabled = store.GetBoolSetting(st, "ha_enabled", cfg.HA.Enabled)
	cfg.HA.HeartbeatIntervalMs = store.GetIntSetting(st, "ha_heartbeat_interval_ms", cfg.HA.HeartbeatIntervalMs)
	cfg.HA.PeerTimeoutMs = store.GetIntSetting(st, "ha_peer_timeout_ms", cfg.HA.PeerTimeoutMs)
	store.GetJSONSetting(st, "ha_peers", &cfg.HA.Peers)

	cfg.Warmup.Enabled = store.GetBoolSetting(st, "warmup_enabled", cfg.Warmup.Enabled)
	cfg.Warmup.IntervalMs = store.GetIntSetting(st, "warmup_interval_ms", cfg.Warmup.IntervalMs)
	cfg.Warmup.KeepAlive = store.GetStringSetting(st, "warmup_keep_alive", cfg.Warmup.KeepAlive)
	store.GetJSONSetting(st, "warmup_models", &cfg.Warmup.Models)

	cfg.CloudBudget.DailyUSDCap = store.GetFloatSetting(st, "cloud_daily_usd_cap", cfg.CloudBudget.DailyUSDCap)
	cfg.CloudBudget.MonthlyUSDCap = store.GetFloatSetting(st, "cloud_monthly_usd_cap", cfg.CloudBudget.MonthlyUSDCap)

	cfg.HuggingFace.Token = store.GetStringSetting(st, "huggingface_token", cfg.HuggingFace.Token)

	store.GetJSONSetting(st, "context_windows", &cfg.ContextWindows)
}

func main() {
	var (
		showVersion   = flag.Bool("version", false, "print version and exit")
		dbFlag        = flag.String("db", "", "path to the SQLite database (overrides MESH_DB_PATH env; default mesh.db)")
		logFormatFlag = flag.String("log-format", "", "log output format: text (default) or json")
		seedNodes     stringSliceFlag
	)
	flag.Var(&seedNodes, "seed-node", `add a node directly to the database and exit, format: "name=...,url=...,runtime=..." (repeatable)`)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ollama-mesh %s - warm-model-aware load balancer with cloud overflow for Ollama\n\n", Version)
		fmt.Fprintf(os.Stderr, "Usage:\n  ollama-mesh [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nNo config file needed: start the binary, then add nodes/API keys/settings\nthrough the dashboard at http://localhost:8080 (admin/admin on first run).\n")
	}
	// Subcommand dispatch: check before flag.Parse() so each subcommand
	// owns its own flag set and does not pollute the main flag namespace.
	if len(os.Args) > 1 && os.Args[1] == "bench" {
		bench.Run(os.Args[2:])
		return
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

	if len(seedNodes) > 0 {
		if err := seedNodesToStore(dbPath, seedNodes); err != nil {
			log.Fatalf("seed nodes: %v", err)
		}
		return
	}

	cfg := &config.Config{}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid default config: %v", err)
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
			log.Fatalf("failed to open store: %v", stErr)
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
			r.PatchNode(name, router.NodePatch{VRAMTotalMB: ov.VRAMTotalMB, GPUModel: ov.GPUModel})
		}
	}
	if drains, err := st.NodeDrainStates(); err == nil {
		for name, ds := range drains {
			if ds.Draining {
				r.DrainNode(name, ds.Reason)
			}
		}
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

	if cfg.HA.Enabled && len(cfg.HA.Peers) > 0 {
		haMonitor := ha.New(cfg.HA)
		adminSrv.SetHAMonitor(haMonitor)
		go haMonitor.Start(ctx)
		log.Printf("Peer health monitor enabled: %d peer(s) (observability only; not failover/HA)", haMonitor.PeerCount())
	}

	proxyHandler := proxy.NewHandler(r, adminSrv, auditLog)
	proxyHandler.SetAuth(authMw)
	proxyHandler.SetAllowManagementEndpoints(cfg.Routing.AllowManagementEndpoints)
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
			Handler:           mux,
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
		case <-sig:
		}
		break
	}
	log.Println("Shutting down gracefully...")

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
	log.Println("Shutdown complete")
}
