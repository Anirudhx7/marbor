package marboragent

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Anirudhx7/marbor/internal/winexit"
)

// defaultRefreshInterval is how often the background Scheduler re-collects
// GPU/host telemetry when --refresh-interval isn't set. GPU temperature/VRAM/
// etc. don't change fast enough to need per-request freshness, so this is
// independent of (and normally much longer than a fraction of) the mesh's
// own node poll interval.
const defaultRefreshInterval = 5 * time.Second

// Run is the marbor-agent binary's entire entry point (called from
// cmd/marbor-agent/main.go). version is the agent's own build version,
// reported back as agent_version so the dashboard can tell which agent build
// a node is running.
//
// "marbor-agent service ..." is dispatched here, before flag.Parse, the
// same way main.go's own subcommand dispatch (e.g. "bench") checks os.Args
// before parsing its own flag set - each subcommand owns its own flags
// without polluting a shared namespace. See service_cmd.go.
func Run(args []string, version string) {
	if len(args) > 0 && args[0] == "service" {
		runServiceCommand(args[1:], version)
		return
	}

	// runAgent's own flag.FlagSet only understands flags - a bare non-flag
	// first argument (e.g. a typo'd "status" meant as "agent service
	// status") is not an error to flag.Parse, which simply stops consuming
	// at the first non-flag token and returns nil (same swallow-class bug
	// as L27's "models bogus" case). Left unchecked, that silently ran the
	// plain foreground agent instead of rejecting the unrecognized
	// subcommand, surfacing a confusing "a token is required" error instead
	// of pointing at the real mistake.
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		winexit.Fatalf("marboragent: unknown agent subcommand %q (did you mean \"service %s\"?)", args[0], args[0])
	}

	if handled, err := runWindowsServiceIfService(func() { runAgent(args, version) }); handled {
		if err != nil {
			winexit.Fatalf("marboragent: windows service execution failed: %v", err)
		}
		return
	}

	runAgent(args, version)
}

func runAgent(args []string, version string) {

	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	port := fs.Int("port", 9200, "port to serve /v1/status and /metrics on")
	refreshInterval := fs.Duration("refresh-interval", defaultRefreshInterval, "how often to re-collect GPU/host telemetry in the background (e.g. 5s, 10s)")
	certFlag := fs.String("cert", "", "TLS certificate file path; if both --cert and --key are set, serves HTTPS instead of plaintext HTTP - set by \"agent service install\", not normally passed by hand")
	keyFlag := fs.String("key", "", "TLS private key file path, paired with --cert")
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "marbor-agent - Marbor Agent: node-local execution point for the mesh\n\n")
		fmt.Fprintf(w, "Usage:\n  marbor-agent --port=<port>   (runs in the foreground; set the MARBOR_AGENT_SECRET env var)\n")
		fmt.Fprintf(w, "  marbor-agent service install --port=<port>\n")
		fmt.Fprintf(w, "                                                     (installs as a persistent OS service)\n")
		fmt.Fprintf(w, "  marbor-agent service {uninstall|start|stop|status}\n\nFlags:\n")
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
		winexit.Fatalf("marboragent: %v", err)
	}

	token := os.Getenv("MARBOR_AGENT_SECRET")
	if token == "" {
		winexit.Fatal("marboragent: a token is required: set the MARBOR_AGENT_SECRET environment variable")
	}
	if *refreshInterval <= 0 {
		winexit.Fatal("marboragent: --refresh-interval must be positive")
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

	// P24: HTTPS iff both --cert and --key are set (normally only true when
	// started by "agent service install", which only ever sets both or
	// neither - see service.Config.args()); otherwise unchanged plaintext.
	// No partial state is possible from flag parsing alone (only one of the
	// two set): that combination falls through to the plaintext branch
	// exactly like neither being set, since the listener can't serve HTTPS
	// with only one of a cert/key pair anyway.
	if *certFlag != "" && *keyFlag != "" {
		log.Printf("marbor-agent %s listening on %s over HTTPS (GET /v1/status, GET /metrics, refreshed every %s)", version, addr, *refreshInterval)
		if err := http.ListenAndServeTLS(addr, *certFlag, *keyFlag, srv.Handler()); err != nil {
			winexit.Fatalf("marboragent: %v", err)
		}
		return
	}

	log.Printf("marbor-agent %s listening on %s (GET /v1/status, GET /metrics, refreshed every %s)", version, addr, *refreshInterval)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}
}
