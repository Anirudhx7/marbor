package cli

import (
	"fmt"
	"io"
	"time"
)

// Version is set at build time via ldflags: -X internal/cli.Version=v0.x.y
// (mirrors main.Version's convention in main.go). Defaults to "dev".
var Version = "dev"

type versionOutput struct {
	ClientVersion   string `json:"client_version"`
	ServerReachable bool   `json:"server_reachable"`
	ServerVersion   string `json:"server_version,omitempty"`
}

// runVersion prints the CLI's own build version, plus a best-effort server
// version via GET /health. Reaching the server is opportunistic here - an
// unreachable server never fails this command, since a local version query
// shouldn't hard-fail on network trouble.
func runVersion(flags *globalFlags, stdout, stderr io.Writer) int {
	out := versionOutput{ClientVersion: Version}

	client := NewClient(flags.server, "")
	client.HTTPClient.Timeout = 3 * time.Second
	if health, err := client.Health(); err == nil {
		out.ServerReachable = true
		out.ServerVersion = health.Version
	}

	if flags.jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitServerError
		}
		return ExitOK
	}

	fmt.Fprintf(stdout, "client version: %s\n", out.ClientVersion)
	if out.ServerReachable {
		fmt.Fprintf(stdout, "server version: %s (%s)\n", out.ServerVersion, flags.server)
	} else {
		fmt.Fprintf(stdout, "server version: unreachable (%s)\n", flags.server)
	}
	return ExitOK
}
