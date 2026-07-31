package cli

import (
	"fmt"
	"io"
	"time"
)

// runStatus implements `mesh status` - GET /health, unauthenticated.
func runStatus(flags *globalFlags, stdout, stderr io.Writer) int {
	client := NewClient(flags.server, "")
	health, err := client.Health()
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, health); handled {
		return code
	}

	uptime := time.Duration(health.UptimeSeconds) * time.Second
	fmt.Fprintf(stdout, "status:   %s\n", health.Status)
	fmt.Fprintf(stdout, "version:  %s\n", health.Version)
	fmt.Fprintf(stdout, "uptime:   %s\n", uptime)
	fmt.Fprintf(stdout, "proxy:    port %d\n", health.ProxyPort)
	fmt.Fprintf(stdout, "nodes:    %d/%d healthy\n", health.Nodes.Healthy, health.Nodes.Total)
	return ExitOK
}
