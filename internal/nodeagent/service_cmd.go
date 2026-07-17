package nodeagent

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent/service"
)

// runServiceCommand dispatches "ollama-mesh agent service <subcommand>".
// Each subcommand owns its own flag set, same pattern as Run's top-level
// dispatch in main.go.
func runServiceCommand(args []string, version string) {
	if len(args) == 0 {
		log.Fatal("nodeagent: usage: ollama-mesh agent service {install|uninstall|start|stop|status}")
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
	default:
		log.Fatalf("nodeagent: unknown service subcommand %q (want install, uninstall, start, stop, or status)", sub)
	}
}

func runServiceInstall(args []string, version string) {
	fs := flag.NewFlagSet("agent service install", flag.ExitOnError)
	port := fs.Int("port", 9200, "port for the installed service to serve /telemetry and /metrics on")
	tokenFlag := fs.String("token", "", "bearer token required on every request (or set the TOKEN env var)")
	refreshInterval := fs.Duration("refresh-interval", 0, "how often the installed service re-collects telemetry (default: the agent's own built-in default)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "ollama-mesh agent service install - register the Node Agent as a persistent, auto-restarting OS service\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  ollama-mesh agent service install --port=<port> --token=<token>\n\n")
		fmt.Fprintf(os.Stderr, "Safe to re-run: re-installing (e.g. after a binary upgrade, or to rotate the token)\n")
		fmt.Fprintf(os.Stderr, "reconfigures and restarts the existing service rather than requiring uninstall first.\n\nFlags:\n")
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

	binaryPath, err := os.Executable()
	if err != nil {
		log.Fatalf("nodeagent: could not resolve the path to this binary: %v", err)
	}

	mgr, err := service.New()
	if err != nil {
		log.Fatalf("nodeagent: %v", err)
	}

	cfg := service.Config{
		BinaryPath:      binaryPath,
		Port:            *port,
		Token:           token,
		RefreshInterval: *refreshInterval,
	}
	if err := mgr.Install(cfg); err != nil {
		log.Fatalf("nodeagent: service install failed: %v", err)
	}
	log.Printf("ollama-mesh agent %s installed as a persistent service (%s), listening on port %d and enabled to restart on boot/failure.", version, service.Name, *port)
}

func runServiceUninstall(args []string) {
	fs := flag.NewFlagSet("agent service uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the installed binary (default: only removes the service registration)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("nodeagent: %v", err)
	}

	mgr, err := service.New()
	if err != nil {
		log.Fatalf("nodeagent: %v", err)
	}
	if err := mgr.Uninstall(*purge); err != nil {
		log.Fatalf("nodeagent: service uninstall failed: %v", err)
	}
	log.Printf("ollama-mesh agent service (%s) uninstalled.", service.Name)
}

func runServiceControl(args []string, action string) {
	mgr, err := service.New()
	if err != nil {
		log.Fatalf("nodeagent: %v", err)
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
		log.Fatalf("nodeagent: service %s failed: %v", action, doErr)
	}
	log.Printf("ollama-mesh agent service (%s) %s.", service.Name, pastTense)
}

func runServiceStatus(args []string) {
	mgr, err := service.New()
	if err != nil {
		log.Fatalf("nodeagent: %v", err)
	}
	status, err := mgr.Status()
	if err != nil {
		log.Fatalf("nodeagent: service status failed: %v", err)
	}
	fmt.Println(status)
}
