package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

// printFirstRunBanner prints the zero-config first-run summary to stdout.
func printFirstRunBanner(fr *config.FirstRunResult, cfgPath string, saved bool) {
	line := "================================================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  ollama-mesh - first run (no config file found)")
	fmt.Println(line)
	fmt.Println()
	if fr.OllamaFound {
		fmt.Printf("  [ok] Local Ollama detected at %s\n", fr.OllamaURL)
		fmt.Println("       Registered as node \"local\". Requests now route through the mesh.")
	} else {
		fmt.Printf("  [!]  No local Ollama detected at %s\n", fr.OllamaURL)
		fmt.Printf("       Starting with zero nodes. Add nodes to %s and restart:\n", cfgPath)
		fmt.Println()
		fmt.Println("         nodes:")
		fmt.Println("           - name: my-gpu")
		fmt.Println("             url: http://<host>:11434")
	}
	fmt.Println()
	fmt.Printf("  Point your apps at:  http://localhost:%d\n", fr.Config.Proxy.Port)
	fmt.Printf("  API key:             %s\n", fr.APIKey)
	fmt.Println()
	fmt.Println("  Dashboard:           http://localhost:8080")
	fmt.Println("  Dashboard login:     see startup log for admin username and password")
	fmt.Println()
	if saved {
		fmt.Printf("  Config saved to %s - your key and token are stable across restarts.\n", cfgPath)
	} else {
		fmt.Printf("  WARNING: could not write %s - key and token are NOT saved and\n", cfgPath)
		fmt.Println("  will be regenerated on the next start.")
	}
	fmt.Println(line)
	fmt.Println()
}

func main() {
	var (
		showVersion   = flag.Bool("version", false, "print version and exit")
		cfgFlag       = flag.String("config", "", "path to config file (overrides CONFIG_PATH env; default config.yaml)")
		validateOnly  = flag.Bool("validate", false, "load and validate the config file, then exit (0 = valid, 1 = invalid)")
		logFormatFlag = flag.String("log-format", "", "log output format: text (default) or json")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ollama-mesh %s - warm-model-aware load balancer with cloud overflow for Ollama\n\n", Version)
		fmt.Fprintf(os.Stderr, "Usage:\n  ollama-mesh [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nWith no config file present, the first run auto-detects local Ollama,\ngenerates keys, and writes config.yaml.\n")
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

	cfgPath := *cfgFlag
	if cfgPath == "" {
		cfgPath = os.Getenv("CONFIG_PATH")
	}
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	// --validate: load and validate the existing config, report, and exit.
	// It never triggers first-run generation - it only checks what is there.
	if *validateOnly {
		if _, vErr := config.LoadConfig(cfgPath); vErr != nil {
			fmt.Fprintf(os.Stderr, "config %s: INVALID: %v\n", cfgPath, vErr)
			os.Exit(1)
		}
		fmt.Printf("config %s: OK\n", cfgPath)
		return
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("Failed to load config: %v", err)
		}
		// Zero-config first run: no config.yaml found.
		fr, frErr := config.GenerateFirstRun(config.DefaultOllamaURL, config.DefaultProbeTimeout)
		if frErr != nil {
			log.Fatalf("First-run setup failed: %v", frErr)
		}
		cfg = fr.Config
		saved := true
		if saveErr := config.SaveConfig(cfgPath, *cfg); saveErr != nil {
			saved = false
			log.Printf("WARNING: could not save %s: %v", cfgPath, saveErr)
			log.Printf("WARNING: continuing with in-memory config; generated keys will change on next restart")
		}
		printFirstRunBanner(fr, cfgPath, saved)
	}

	// Configure structured logging. CLI flag takes precedence over config file.
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
	log.Printf("Proxy port      : %d", cfg.Proxy.Port)
	log.Printf("Auth enabled    : %t", cfg.Auth.IsEnabled())
	log.Printf("Metrics port    : %d", cfg.Metrics.Port)
	log.Printf("Poll interval   : %dms", cfg.Routing.PollIntervalMs)
	log.Printf("Nodes registered: %d", len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		log.Printf("  - %s (%s) -> %s", n.Name, n.GPUModel, n.URL)
	}

	// Loud, unmissable warning when the proxy is running without authentication.
	// auth.enabled defaults to the bool zero value (false), so a hand-written
	// config that omits the flag silently exposes an unauthenticated LLM gateway
	// that forwards to backend GPU nodes. First-run configs always set it true;
	// this catches the dangerous omit-the-flag case.
	if !cfg.Auth.IsEnabled() {
		log.Printf("WARNING: ================================================================")
		log.Printf("WARNING: AUTHENTICATION IS DISABLED - every request is forwarded with no")
		log.Printf("WARNING: API key check. Anyone who can reach :%d has full access to your", cfg.Proxy.Port)
		log.Printf("WARNING: backend models. Set 'auth: {enabled: true}' with at least one key")
		log.Printf("WARNING: for any non-loopback or shared deployment.")
		log.Printf("WARNING: ================================================================")
	}

	// Open the SQLite persistence store. "-" disables; empty defaults to mesh.db.
	var st store.Store = store.NopStore{}
	if cfg.Storage.DBPath != "-" {
		var stErr error
		st, stErr = store.Open(cfg.Storage.DBPath)
		if stErr != nil {
			log.Fatalf("failed to open store: %v", stErr)
		}
		defer st.Close()
	}

	authMw := auth.NewMiddleware(cfg.Auth)

	// Per-key usage/quota counters persist across restarts via SQLite.
	if err := authMw.LoadFromStore(st); err != nil {
		log.Printf("WARNING: could not restore key counters from store: %v", err)
	}

	// Overlay runtime API keys from the store (keys added via admin API that
	// aren't in config.yaml). Config-file keys always win on name collision
	// because NewMiddleware already loaded them; AddKey is idempotent on dup key token.
	if runtimeKeys, err := st.AllKeys(); err == nil {
		for _, k := range runtimeKeys {
			if k.Revoked {
				authMw.RevokeKey(k.Name)
			} else {
				authMw.AddKey(config.KeyConfig{
					Name:         k.Name,
					Key:          k.Key,
					RateLimit:    k.RateLimit,
					DailyLimit:   k.DailyLimit,
					MonthlyLimit: k.MonthlyLimit,
					Models:       k.Models,
				})
			}
		}
		if len(runtimeKeys) > 0 {
			log.Printf("store: loaded %d runtime key(s)", len(runtimeKeys))
		}
	} else {
		log.Printf("WARNING: could not load runtime keys from store: %v", err)
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

	// Overlay runtime nodes from the store (nodes added via admin API).
	// Nodes already in config are already registered; AddNode is called for
	// runtime-only nodes. Overrides and drain states are applied to all nodes.
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
		}
		if len(runtimeNodes) > 0 {
			log.Printf("store: loaded %d runtime node(s)", len(runtimeNodes))
		}
	} else {
		log.Printf("WARNING: could not load runtime nodes from store: %v", err)
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

	// Load persisted timezone from the KV store.
	if tzVal, err := st.GetSetting("timezone"); err == nil && tzVal != "" {
		cfg.Timezone = tzVal
		r.SetTimezone(tzVal)
		log.Printf("store: loaded timezone %q", tzVal)
	}

	// Load persisted routing strategy from the KV store.
	if stratVal, err := st.GetSetting("routing_strategy"); err == nil && stratVal != "" {
		if err := r.SetStrategy(stratVal); err != nil {
			log.Printf("store: ignoring invalid persisted routing strategy %q: %v", stratVal, err)
		} else {
			log.Printf("store: loaded routing strategy %q", stratVal)
		}
	}

	// Load remaining Settings-page scalars persisted via the Phase 2 SQLite
	// migration (see admin.go handleUpdateSettings), applying them over
	// config.yaml's value before the servers below start listening.
	if v, err := st.GetSetting("proxy_port"); err == nil && v != "" {
		if port, convErr := strconv.Atoi(v); convErr == nil && port > 0 {
			cfg.Proxy.Port = port
		}
	}
	if v, err := st.GetSetting("proxy_log_format"); err == nil && v != "" {
		cfg.Proxy.LogFormat = v
	}
	if v, err := st.GetSetting("litellm_url"); err == nil && v != "" {
		cfg.LiteLLM.URL = v
	}
	if v, err := st.GetSetting("litellm_enabled"); err == nil && v != "" {
		cfg.LiteLLM.Enabled = v == "true"
	}
	if v, err := st.GetSetting("cloud_daily_usd_cap"); err == nil && v != "" {
		if cap, convErr := strconv.ParseFloat(v, 64); convErr == nil {
			cfg.CloudBudget.DailyUSDCap = cap
		}
	}
	if v, err := st.GetSetting("cloud_monthly_usd_cap"); err == nil && v != "" {
		if cap, convErr := strconv.ParseFloat(v, 64); convErr == nil {
			cfg.CloudBudget.MonthlyUSDCap = cap
		}
	}
	if v, err := st.GetSetting("metrics_enabled"); err == nil && v != "" {
		cfg.Metrics.Enabled = v == "true"
	}
	if v, err := st.GetSetting("metrics_port"); err == nil && v != "" {
		if port, convErr := strconv.Atoi(v); convErr == nil && port > 0 {
			cfg.Metrics.Port = port
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

	// Load persisted routing rules (fixes audit finding #30).
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
	adminSrv.SetConfigPath(cfgPath)
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
	accessEnabled := cfg.Proxy.AccessLog == nil || *cfg.Proxy.AccessLog
	proxyHandler.SetAccessLogger(proxy.NewAccessLogger(os.Stdout, accessEnabled))
	log.Printf("Access log     : %t (stdout, JSON lines)", accessEnabled)
	// RecoverMiddleware is the outermost wrapper so a handler panic returns a
	// clean 500 and increments a metric instead of an ugly connection drop.
	wrapped := proxy.RecoverMiddleware(authMw.Handler(proxyHandler))

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
		Handler:           proxy.RecoverMiddleware(adminSrv.Handler()),
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
			newCfg, err := config.LoadConfig(cfgPath)
			if err != nil {
				log.Printf("config reload failed: %v (keeping previous config)", err)
				continue
			}
			authMw.Reload(newCfg.Auth)
			r.SetWarmupConfig(newCfg.Warmup)
			r.SetClouds(newCfg.CloudProviders)
			added, removed := r.SyncNodes(newCfg.Nodes)
			log.Printf("config reloaded from %s (auth keys: %d, warmup: %v, nodes: +%d/-%d, cloud providers: %d)",
				cfgPath, len(newCfg.Auth.Keys), newCfg.Warmup.Enabled, added, removed, len(newCfg.CloudProviders))
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
