package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/ha"
	"github.com/ollama-mesh/ollama-mesh/internal/proxy"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
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
	fmt.Printf("  Admin token:         %s\n", fr.AdminToken)
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
		showVersion  = flag.Bool("version", false, "print version and exit")
		cfgFlag      = flag.String("config", "", "path to config file (overrides CONFIG_PATH env; default config.yaml)")
		validateOnly = flag.Bool("validate", false, "load and validate the config file, then exit (0 = valid, 1 = invalid)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ollama-mesh %s - warm-model-aware load balancer with cloud overflow for Ollama\n\n", Version)
		fmt.Fprintf(os.Stderr, "Usage:\n  ollama-mesh [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nWith no config file present, the first run auto-detects local Ollama,\ngenerates keys, and writes config.yaml.\n")
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

	log.Printf("ollama-mesh %s starting...", Version)
	log.Printf("Proxy port      : %d", cfg.Proxy.Port)
	log.Printf("Auth enabled    : %t", cfg.Auth.Enabled)
	log.Printf("Metrics port    : %d", cfg.Metrics.Port)
	log.Printf("Poll interval   : %dms", cfg.Routing.PollIntervalMs)
	log.Printf("Nodes registered: %d", len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		log.Printf("  - %s (%s) -> %s", n.Name, n.GPUModel, n.URL)
	}

	authMw := auth.NewMiddleware(cfg.Auth)

	// Per-key usage/quota counters persist across restarts. "-" disables.
	statePath := cfg.Auth.StatePath
	if statePath == "-" {
		statePath = ""
	}
	if err := authMw.LoadState(statePath); err != nil {
		log.Printf("WARNING: could not load usage state from %s: %v", statePath, err)
	}

	r := router.New(cfg.Routing, cfg.Nodes, cfg.CloudProviders)
	r.SetDockerConfig(cfg.Docker)
	if cfg.Docker.Enabled {
		log.Printf("Docker auto-discovery enabled (socket: %s)", cfg.Docker.Socket)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Start(ctx)

	// Periodically flush usage counters so a restart preserves quota/usage
	// state (crash loses at most one interval).
	if statePath != "" {
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := authMw.SaveState(statePath); err != nil {
						log.Printf("WARNING: usage state flush failed: %v", err)
					}
				}
			}
		}()
	}

	auditLog, err := audit.New(func() string {
		if cfg.Audit.Enabled {
			return cfg.Audit.Path
		}
		return ""
	}())
	if err != nil {
		log.Fatalf("audit log: %v", err)
	}
	defer auditLog.Close()

	adminSrv := admin.NewServer(r, authMw, *cfg)
	adminSrv.SetAuditLogger(auditLog)
	adminSrv.SetVersion(Version)
	adminSrv.SetConfigPath(cfgPath)

	if cfg.HA.Enabled && len(cfg.HA.Peers) > 0 {
		haMonitor := ha.New(cfg.HA)
		adminSrv.SetHAMonitor(haMonitor)
		go haMonitor.Start(ctx)
		log.Printf("HA enabled: monitoring %d peer(s)", haMonitor.PeerCount())
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
	<-sig
	log.Println("Shutting down gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	// Final flush so the just-served requests are not lost on restart.
	if statePath != "" {
		if err := authMw.SaveState(statePath); err != nil {
			log.Printf("WARNING: final usage state flush failed: %v", err)
		}
	}
	log.Println("Shutdown complete")
}
