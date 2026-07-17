package nodeagent

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

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

	srv := &Server{Token: token, Version: version}
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("ollama-mesh agent %s listening on %s (GET /telemetry, GET /metrics)", version, addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("nodeagent: %v", err)
	}
}
