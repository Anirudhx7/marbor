// ollama-mesh-agent is the Node Agent binary: the node-local execution point
// the mesh polls for GPU/host/runtime telemetry and issues control actions
// through (internal/nodeagent). It deliberately imports nothing from the
// control-plane side of the codebase (internal/admin, internal/router,
// internal/store, internal/proxy, internal/auth, internal/cli) - a host
// running only this binary has no code path capable of starting the Mesh
// control plane, opening mesh.db, or serving the admin API. See
// .local/specs/node-agent.md for the agent's design and
// internal/nodeagent/service for how it registers itself as a persistent OS
// service.
//
// Unlike the pre-split combined binary, this binary IS the agent - there is
// no "agent" subcommand to type first: "ollama-mesh-agent service install
// --port=9200" and "ollama-mesh-agent --port=9200" replace what used to be
// "ollama-mesh agent service install ..." and "ollama-mesh agent ...".
package main

import (
	"fmt"
	"os"

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
)

// Version is set at build time via ldflags: -X main.Version=v0.x.y (the same
// mechanism and the same release version as the ollama-mesh binary - both
// artifacts are built from one tag, see .goreleaser.yaml).
var Version = "dev"

func main() {
	// Handled here, not inside internal/nodeagent: -version is a top-level
	// binary concern (install.sh/install.ps1 both run "<binary> -version"
	// to detect upgrades - the same pattern the root ollama-mesh binary
	// uses), not an agent runtime flag, and nodeagent.Run's own flag set
	// (runAgent, service_cmd.go) has no reason to know about it.
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Printf("ollama-mesh-agent %s\n", Version)
		return
	}
	nodeagent.Run(os.Args[1:], Version)
}
