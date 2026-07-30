package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

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
	port := fs.Int("port", 9200, "port for the installed service to serve /v1/status and /metrics on")
	tokenFlag := fs.String("token", "", "bearer token required on every request (or set the TOKEN env var)")
	enrollFlag := fs.String("enroll", "", "one-time enrollment code from the mesh admin UI, exchanged for the real token (or set the ENROLL env var); requires --mesh")
	meshFlag := fs.String("mesh", "", "mesh admin base URL, required together with --enroll (or set the MESH env var)")
	refreshInterval := fs.Duration("refresh-interval", 0, "how often the installed service re-collects telemetry (default: the agent's own built-in default)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "ollama-mesh agent service install - register the Node Agent as a persistent, auto-restarting OS service\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  ollama-mesh agent service install --port=<port> --token=<token>\n")
		fmt.Fprintf(os.Stderr, "  ollama-mesh agent service install --port=<port> --enroll=<code> --mesh=<url>\n\n")
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
	enroll := *enrollFlag
	if enroll == "" {
		enroll = os.Getenv("ENROLL")
	}
	mesh := *meshFlag
	if mesh == "" {
		mesh = os.Getenv("MESH")
	}
	if token == "" && enroll == "" {
		log.Fatal("nodeagent: a token is required: pass --token=<token>/TOKEN env var, or --enroll=<code>/ENROLL env var with --mesh=<url>/MESH env var")
	}
	if token == "" {
		if mesh == "" {
			log.Fatal("nodeagent: --enroll requires --mesh=<mesh admin base URL> (or the MESH environment variable)")
		}
		exchanged, err := exchangeEnrollmentCode(mesh, enroll)
		if err != nil {
			log.Fatalf("nodeagent: enrollment failed: %v", err)
		}
		token = exchanged
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

// exchangeEnrollmentCode calls the mesh's POST /admin/agent/enroll endpoint
// to trade a short-lived, single-use enrollment code for the node's real,
// permanent bearer token (P50). This is the agent's first-ever outbound
// call to the mesh - normally the mesh polls the agent, never the reverse -
// so meshBaseURL must be supplied explicitly by the operator (via --mesh or
// the MESH env var); the agent has no other way to know the mesh's address.
func exchangeEnrollmentCode(meshBaseURL, code string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reqBody, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(meshBaseURL, "/") + "/admin/agent/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach mesh at %s: %w", meshBaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mesh rejected enrollment code (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("mesh returned an empty token")
	}
	return out.Token, nil
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
