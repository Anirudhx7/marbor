package nodeagent

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/winexit"
)

// defaultRefreshInterval is how often the background Scheduler re-collects
// GPU/host telemetry when --refresh-interval isn't set. GPU temperature/VRAM/
// etc. don't change fast enough to need per-request freshness, so this is
// independent of (and normally much longer than a fraction of) the mesh's
// own node poll interval.
const defaultRefreshInterval = 5 * time.Second

// warnIfTokenFlagUsed prints a deprecation notice to w when the caller passed
// --token directly (tokenFlag != ""), never for the TOKEN env var or the
// --enroll/--mesh flow. Shared by runAgent and runServiceInstall
// (service_cmd.go). Written to stderr (via the caller passing os.Stderr),
// not through the logger: this is CLI UX aimed at whoever typed the flag,
// not a runtime log line that should get timestamped or land in a service
// log when the agent runs under systemd/launchd/Windows service.
func warnIfTokenFlagUsed(w io.Writer, tokenFlag string) {
	if tokenFlag == "" {
		return
	}
	fmt.Fprintln(w, "warning: --token is deprecated and will be removed in the next major release. Use the TOKEN environment variable or the ENROLL flow instead.")
}

// Run is the "ollama-mesh agent" subcommand entry point (called from main.go
// the same way "ollama-mesh bench" dispatches to internal/bench.Run) - same
// binary, same cross-compile targets as the mesh itself, per the build spec.
// version is the mesh binary's own build version (main.Version), reported
// back as agent_version so the dashboard can tell which agent build a node
// is running.
//
// "ollama-mesh agent service ..." is dispatched here, before flag.Parse, the
// same way main.go's own subcommand dispatch (e.g. "bench") checks os.Args
// before parsing its own flag set - each subcommand owns its own flags
// without polluting a shared namespace. See service_cmd.go.
func Run(args []string, version string) {
	if len(args) > 0 && args[0] == "service" {
		runServiceCommand(args[1:], version)
		return
	}

	if handled, err := runWindowsServiceIfService(func() { runAgent(args, version) }); handled {
		if err != nil {
			winexit.Fatalf("nodeagent: windows service execution failed: %v", err)
		}
		return
	}

	runAgent(args, version)
}

func runAgent(args []string, version string) {

	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	port := fs.Int("port", 9200, "port to serve /v1/status and /metrics on")
	tokenFlag := fs.String("token", "", "bearer token required on every request (deprecated, use the TOKEN env var instead)")
	refreshInterval := fs.Duration("refresh-interval", defaultRefreshInterval, "how often to re-collect GPU/host telemetry in the background (e.g. 5s, 10s)")
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "ollama-mesh agent - Node Agent: node-local execution point for the mesh\n\n")
		fmt.Fprintf(w, "Usage:\n  ollama-mesh agent --port=<port>   (runs in the foreground; set the TOKEN env var)\n")
		fmt.Fprintf(w, "  ollama-mesh agent service install --port=<port>\n")
		fmt.Fprintf(w, "                                                     (installs as a persistent OS service)\n")
		fmt.Fprintf(w, "  ollama-mesh agent service {uninstall|start|stop|status}\n\nFlags:\n")
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	fs.Usage = func() { usage(os.Stderr) }

	// -h/--help must be intercepted before fs.Parse runs, same reasoning as
	// internal/bench.Run and internal/cli's parseFlags: flag's own usage hook
	// fires identically for a real bad-flag error and for a help request.
	for _, a := range args {
		if a == "-h" || a == "--help" {
			usage(os.Stdout)
			return
		}
	}
	if err := fs.Parse(args); err != nil {
		winexit.Fatalf("nodeagent: %v", err)
	}

	warnIfTokenFlagUsed(os.Stderr, *tokenFlag)
	token := *tokenFlag
	if token == "" {
		token = os.Getenv("TOKEN")
	}
	if token == "" {
		winexit.Fatal("nodeagent: a token is required: pass --token=<token> or set the TOKEN environment variable")
	}
	if *refreshInterval <= 0 {
		winexit.Fatal("nodeagent: --refresh-interval must be positive")
	}

	// Start the HTTP server before building/seeding the scheduler so the
	// listener binds within Windows SCM's 30-second start timeout (error
	// 1053). The full blocking startup chain is:
	//   NewScheduler        : up to 15 s (5 s GPU detect + 10 s runtime detect)
	//   Seed/refresh        : up to 20 s (GPU + host + runtime probes)
	// Together that exceeds the 30 s deadline on a cold machine.
	//
	// Server.Snapshot() handles the pre-construction window safely: it
	// returns metadata-only (node_id/version/capabilities/platform) with nil
	// GPU/Host/Runtime blocks until the Scheduler is wired up and Seed has
	// run. The mesh poller treats nil blocks as "not yet collected."
	srv := &Server{Token: token, Version: version}
	addr := fmt.Sprintf(":%d", *port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Build, seed, and refresh the scheduler entirely in the background so
	// ListenAndServe can bind (and signal SCM) without waiting.
	go func() {
		sched := NewScheduler(version)
		sched.Seed()
		srv.SetScheduler(sched) // atomic store - safe concurrent read by HTTP handlers
		sched.Start(ctx, *refreshInterval)
	}()

	log.Printf("ollama-mesh agent %s listening on %s (GET /v1/status, GET /metrics, refreshed every %s)", version, addr, *refreshInterval)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		winexit.Fatalf("nodeagent: %v", err)
	}
}
