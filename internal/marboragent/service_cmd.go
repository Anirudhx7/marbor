package marboragent

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent/service"
	"github.com/Anirudhx7/marbor/internal/winexit"
)

// runServiceCommand dispatches "marbor-agent service <subcommand>".
// Each subcommand owns its own flag set, same pattern as Run's top-level
// dispatch in main.go.
func runServiceCommand(args []string, version string) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage: marbor-agent service {install|uninstall|start|stop|status|regen-cert} [flags]")
		fmt.Println(`Run "marbor-agent service <subcommand> --help" for flags specific to that subcommand.`)
		return
	}
	if len(args) == 0 {
		winexit.Fatal("marboragent: usage: marbor-agent service {install|uninstall|start|stop|status|regen-cert}")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		runServiceInstall(rest, version)
	case "uninstall":
		runServiceUninstall(rest)
	case "start":
		runServiceControl(rest, "start")
	case "stop":
		runServiceControl(rest, "stop")
	case "status":
		runServiceStatus(rest)
	case "regen-cert":
		runServiceRegenCert(rest)
	default:
		winexit.Fatalf("marboragent: unknown service subcommand %q (want install, uninstall, start, stop, status, or regen-cert)", sub)
	}
}

func runServiceInstall(args []string, version string) {
	fs := flag.NewFlagSet("agent service install", flag.ExitOnError)
	port := fs.Int("port", 9200, "port for the installed service to serve /v1/status and /metrics on")
	enrollFlag := fs.String("enroll", "", "one-time enrollment code from the marbor admin UI, exchanged for the real token (or set the MARBOR_ENROLL env var); requires --server")
	serverFlag := fs.String("server", "", "marbor admin base URL, required together with --enroll (or set the MARBOR_SERVER env var)")
	refreshInterval := fs.Duration("refresh-interval", 0, "how often the installed service re-collects telemetry (default: the agent's own built-in default)")
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "marbor-agent service install - register the Marbor Agent as a persistent, auto-restarting OS service\n\n")
		fmt.Fprintf(w, "Usage:\n  marbor-agent service install --port=<port>   (set the MARBOR_AGENT_SECRET env var)\n")
		fmt.Fprintf(w, "  marbor-agent service install --port=<port> --enroll=<code> --server=<url>\n\n")
		fmt.Fprintf(w, "Safe to re-run: re-installing (e.g. after a binary upgrade, or to rotate the token)\n")
		fmt.Fprintf(w, "reconfigures and restarts the existing service rather than requiring uninstall first.\n\nFlags:\n")
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	fs.Usage = func() { usage(os.Stderr) }

	// -h/--help must be intercepted before fs.Parse runs, same reasoning as
	// runAgent/bench.Run: flag's own usage hook fires identically for a real
	// bad-flag error and for a help request.
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
	enroll := *enrollFlag
	if enroll == "" {
		enroll = os.Getenv("MARBOR_ENROLL")
	}
	server := *serverFlag
	if server == "" {
		server = os.Getenv("MARBOR_SERVER")
	}

	if token == "" && enroll == "" {
		winexit.Fatal("marboragent: a token is required: set the MARBOR_AGENT_SECRET env var, or --enroll=<code>/MARBOR_ENROLL env var with --server=<url>/MARBOR_SERVER env var")
	}
	// An explicit enrollment code always takes precedence over an existing
	// MARBOR_AGENT_SECRET - an operator running --enroll is deliberately
	// rotating the token, and silently keeping the old one instead would
	// burn the single-use code while reporting success (P280).
	if enroll != "" {
		if server == "" {
			winexit.Fatal("marboragent: --enroll requires --server=<marbor admin base URL> (or the MARBOR_SERVER environment variable)")
		}
		exchanged, err := exchangeEnrollmentCode(server, enroll)
		if err != nil {
			winexit.Fatalf("marboragent: enrollment failed: %v", err)
		}
		token = exchanged
	}

	binaryPath, err := os.Executable()
	if err != nil {
		winexit.Fatalf("marboragent: could not resolve the path to this binary: %v", err)
	}

	mgr, err := service.New()
	if err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}
	cfg := service.Config{
		BinaryPath:      binaryPath,
		Port:            *port,
		Token:           token,
		RefreshInterval: *refreshInterval,
	}
	if err := mgr.Install(cfg); err != nil {
		winexit.Fatalf("marboragent: service install failed: %v", err)
	}
	log.Printf("marbor-agent %s installed as a persistent service (%s), listening on port %d and enabled to restart on boot/failure.", version, service.Name, *port)
}

// exchangeEnrollmentCode calls the marbor's POST /admin/agent/enroll endpoint
// to trade a short-lived, single-use enrollment code for the node's real,
// permanent bearer token (P50). This is the agent's first-ever outbound
// call to the marbor - normally marbor polls the agent, never the reverse -
// so serverBaseURL must be supplied explicitly by the operator (via --server or
// the MARBOR_SERVER env var); the agent has no other way to know the marbor's address.
func exchangeEnrollmentCode(serverBaseURL, code string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reqBody, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(serverBaseURL, "/") + "/admin/agent/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach marbor at %s: %w", serverBaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("marbor rejected enrollment code (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("marbor returned an empty token")
	}
	return out.Token, nil
}

func runServiceUninstall(args []string) {
	fs := flag.NewFlagSet("agent service uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the installed binary (default: only removes the service registration)")

	// -h/--help must be intercepted before fs.Parse runs (same reasoning as
	// runServiceInstall above); this FlagSet has no custom fs.Usage, so fall
	// back to flag's own default usage output, just routed to stdout.
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return
		}
	}
	if err := fs.Parse(args); err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}

	mgr, err := service.New()
	if err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}
	if err := mgr.Uninstall(*purge); err != nil {
		winexit.Fatalf("marboragent: service uninstall failed: %v", err)
	}
	log.Printf("marbor-agent service (%s) uninstalled.", service.Name)
}

func runServiceControl(args []string, action string) {
	// action never had its own flag.FlagSet, so it never recognized -h/--help
	// at all - args was silently ignored entirely, meaning "agent service
	// stop --help" used to skip straight to actually stopping the real
	// installed OS service instead of showing help. Any other unexpected
	// argument is now also rejected rather than silently ignored.
	for _, a := range args {
		if a == "-h" || a == "--help" {
			verb := "Starts"
			if action == "stop" {
				verb = "Stops"
			}
			fmt.Printf("Usage: marbor-agent service %s\n\n%s the already-installed Marbor Agent OS service. Takes no flags.\n", action, verb)
			return
		}
	}
	if len(args) > 0 {
		winexit.Fatalf("marboragent: agent service %s takes no arguments (got %q)", action, args[0])
	}

	mgr, err := service.New()
	if err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}
	var doErr error
	var pastTense string
	if action == "start" {
		doErr = mgr.Start()
		pastTense = "started"
	} else {
		doErr = mgr.Stop()
		pastTense = "stopped"
	}
	if doErr != nil {
		winexit.Fatalf("marboragent: service %s failed: %v", action, doErr)
	}
	log.Printf("marbor-agent service (%s) %s.", service.Name, pastTense)
}

func runServiceStatus(args []string) {
	// Same bug class as runServiceControl (fixed above): this never had its
	// own flag.FlagSet and used to silently ignore args entirely, so
	// "agent service status --help" ran the real mgr.Status() call instead
	// of showing help - it happened to still exit 0 because Status()
	// succeeds even when unwanted, which made the bug easy to miss.
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println("Usage: marbor-agent service status")
			fmt.Println("\nPrints the installed Marbor Agent OS service's current status. Takes no flags.")
			return
		}
	}
	if len(args) > 0 {
		winexit.Fatalf("marboragent: agent service status takes no arguments (got %q)", args[0])
	}

	mgr, err := service.New()
	if err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}
	status, err := mgr.Status()
	if err != nil {
		winexit.Fatalf("marboragent: service status failed: %v", err)
	}
	fmt.Println(status)

	// P24: print the TLS certificate's fingerprint if one exists, so the
	// operator can compare it against what the marbor's tls-probe endpoint
	// shows before confirming a pin (spec section 1 step 3). Silent (not a
	// fatal error) when no cert exists yet - most nodes are plaintext and
	// this is a status display, not a requirement.
	//
	// Skipped when the service isn't installed: Uninstall never deletes the
	// cert/key files (they're left in place so a future re-install keeps the
	// same pinned fingerprint), so printing one here would show a fingerprint
	// for a service that status just reported as absent.
	if status != "not installed" {
		certPath, _ := service.CertKeyPaths()
		if fingerprint, err := service.AgentCertFingerprint(certPath); err == nil {
			fmt.Printf("TLS certificate fingerprint: %s\n", fingerprint)
		}
	}
}

// runServiceRegenCert forcibly regenerates the installed service's TLS
// certificate/key (P24, spec section 4 - suspected key compromise or
// planned operator-driven rotation, never automatic). This deliberately
// invalidates whatever fingerprint the marbor currently has pinned for this
// node: the operator must re-run the marbor-side confirm-and-pin flow
// afterward (spec sections 1/2/6) - regenerating here does not, and must
// not, notify or update marbor itself.
func runServiceRegenCert(args []string) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println("Usage: marbor-agent service regen-cert")
			fmt.Println("\nForcibly regenerates the installed Marbor Agent's TLS certificate and key,")
			fmt.Println("then restarts the service so the new certificate takes effect. This")
			fmt.Println("invalidates any fingerprint marbor has pinned for this node - re-run the")
			fmt.Println("marbor's confirm-and-pin flow afterward. Takes no flags.")
			return
		}
	}
	if len(args) > 0 {
		winexit.Fatalf("marboragent: agent service regen-cert takes no arguments (got %q)", args[0])
	}

	certPath, keyPath := service.CertKeyPaths()
	if err := service.EnsureAgentCert(certPath, keyPath, true); err != nil {
		winexit.Fatalf("marboragent: regenerating TLS certificate failed: %v", err)
	}

	mgr, err := service.New()
	if err != nil {
		winexit.Fatalf("marboragent: %v", err)
	}
	// Ignore Stop's error (matches the install/uninstall precedent elsewhere
	// in this file) - the service may not currently be running, which isn't
	// itself a failure of this command.
	_ = mgr.Stop()
	if err := mgr.Start(); err != nil {
		winexit.Fatalf("marboragent: regenerated the TLS certificate but restarting the service failed: %v", err)
	}

	fingerprint, err := service.AgentCertFingerprint(certPath)
	if err != nil {
		// The regeneration and restart already succeeded; failing to read
		// back the fingerprint for display is not itself a command failure.
		log.Printf("marbor-agent service (%s) TLS certificate regenerated and service restarted, but could not read back the new fingerprint: %v", service.Name, err)
		return
	}
	log.Printf("marbor-agent service (%s) TLS certificate regenerated (new fingerprint: %s) and service restarted. Re-confirm and re-pin this node from the marbor admin UI/CLI.", service.Name, fingerprint)
}
