package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/proxy"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("ollama-mesh v0.1.0 starting...")
	log.Printf("Proxy port      : %d", cfg.Proxy.Port)
	log.Printf("Auth enabled    : %t", cfg.Auth.Enabled)
	log.Printf("Metrics port    : %d", cfg.Metrics.Port)
	log.Printf("Poll interval   : %dms", cfg.Routing.PollIntervalMs)
	log.Printf("Nodes registered: %d", len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		log.Printf("  - %s (%s) -> %s", n.Name, n.GPUModel, n.URL)
	}

	authMw := auth.NewMiddleware(cfg.Auth)

	r := router.New(cfg.Routing, cfg.Nodes, cfg.CloudProviders)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Start(ctx)

	adminSrv := admin.NewServer(r, authMw, *cfg)

	proxyHandler := proxy.NewHandler(r, adminSrv)
	wrapped := authMw.Handler(proxyHandler)

	proxySrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Proxy.Port),
		Handler:      wrapped,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	var metricsSrv *http.Server
	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsSrv = &http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.Metrics.Port),
			Handler: mux,
		}
		go func() {
			log.Printf("Metrics server listening on :%d/metrics", cfg.Metrics.Port)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Metrics server error: %v", err)
			}
		}()
	}

	adminHttpSrv := &http.Server{
		Addr:         ":8080",
		Handler:      adminSrv.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		log.Printf("Admin dashboard available at http://localhost:8080")
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
	log.Println("Shutdown complete")
}
