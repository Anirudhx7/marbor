package nodeagent

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// defaultRefreshInterval is how often the background Scheduler re-collects
// GPU/host telemetry when --refresh-interval isn't set. GPU temperature/VRAM/
// etc. don't change fast enough to need per-request freshness, so this is
// independent of (and normally much longer than a fraction of) the mesh's
// own node poll interval.
const defaultRefreshInterval = 5 * time.Second

// Run is the "ollama-mesh agent" subcommand entry point (called from main.go
// the same way "ollama-mesh bench" dispatches to internal/bench.Run) - same
// binary, same cross-compile targets as the mesh itself, per the build spec.
// version is the mesh binary's own build version (main.Version), reported
// back as agent_version so the dashboard can tell which agent build a node
// is running.
func Run(args []string, version string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	port := fs.Int("port", 9200, "port to serve /telemetry and /metrics on")
	tokenFlag := fs.String("token", "", "bearer token required on every request (or set the TOKEN env var)")
	refreshInterval := fs.Duration("refresh-interval", defaultRefreshInterval, "how often to re-collect GPU/host telemetry in the background (e.g. 5s, 10s)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "ollama-mesh agent - Node Agent: serves GPU/host telemetry for the mesh to poll\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  ollama-mesh agent --port=<port> --token=<token>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatalf("nodeagent: %v", err)
	}

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("TOKEN")
	}
	if token == "" {
		log.Fatal("nodeagent: a token is required: pass --token=<token> or set the TOKEN environment variable")
	}
	if *refreshInterval <= 0 {
		log.Fatal("nodeagent: --refresh-interval must be positive")
	}

	// Seed synchronously so the very first request never observes an
	// empty/never-collected cache, then hand refreshing off to a background
	// goroutine - GET /telemetry and GET /metrics only ever read the cache
	// (see scheduler.go, server.go), never fork nvidia-smi on the request
	// path. NewScheduler also does GPU vendor detection once here, at
	// startup - see gpu.go.
	scheduler := NewScheduler(version)
	scheduler.Seed()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Start(ctx, *refreshInterval)

	srv := &Server{Token: token, Version: version, Scheduler: scheduler}
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("ollama-mesh agent %s listening on %s (GET /telemetry, GET /metrics, refreshed every %s)", version, addr, *refreshInterval)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("nodeagent: %v", err)
	}
}
