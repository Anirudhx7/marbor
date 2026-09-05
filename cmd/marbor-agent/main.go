// marbor-agent is the Marbor Agent binary: the node-local execution point
// marbor polls for GPU/host/runtime telemetry and issues control actions
// through (internal/marboragent). It deliberately imports nothing from the
// control-plane side of the codebase (internal/admin, internal/router,
// internal/store, internal/proxy, internal/auth, internal/cli) - a host
// running only this binary has no code path capable of starting the Marbor
// control plane, opening marbor.db, or serving the admin API. See
// internal/marboragent/service for how it registers itself as a persistent OS
// service.
//
// Unlike the pre-split combined binary, this binary IS the agent - there is
// no "agent" subcommand to type first: "marbor-agent service install
// --port=9200" and "marbor-agent --port=9200" replace what used to be
// "marbor agent service install ..." and "marbor agent ...".
package main

import (
	"fmt"
	"os"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// Version is set at build time via ldflags: -X main.Version=v0.x.y (the same
// mechanism and the same release version as the marbor binary - both
// artifacts are built from one tag, see .goreleaser.yaml).
var Version = "dev"

func main() {
	// Handled here, not inside internal/marboragent: -version is a top-level
	// binary concern (install.sh/install.ps1 both run "<binary> -version"
	// to detect upgrades - the same pattern the root marbor binary
	// uses), not an agent runtime flag, and marboragent.Run's own flag set
	// (runAgent, service_cmd.go) has no reason to know about it.
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Printf("marbor-agent %s\n", Version)
		return
	}
	marboragent.Run(os.Args[1:], Version)
}
