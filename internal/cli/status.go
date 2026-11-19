package cli

import (
	"fmt"
	"io"
	"time"
)

// runStatus implements `marbor status` - GET /health, unauthenticated.
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
	fmt.Fprintf(stdout, "nodes:    %d/%d healthy%s\n", health.Nodes.Healthy, health.Nodes.Total, workerProbeSuffix(health.Nodes.Workers))
	return ExitOK
}

// workerProbeSuffix names replica workers whose probe is failing. They are not
// part of the healthy count, so the line says why a node is missing from it.
func workerProbeSuffix(workers int) string {
	switch {
	case workers <= 0:
		return ""
	case workers == 1:
		return ", 1 worker probe failing"
	default:
		return fmt.Sprintf(", %d workers probe failing", workers)
	}
}
