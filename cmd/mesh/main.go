// mesh is a thin CLI client of the ollama-mesh Admin API.
//
// Usage:
//
//	mesh <command> [flags]
//
// Commands: version, status, nodes, models. See `mesh --help`.
package main

import (
	"os"

	"github.com/ollama-mesh/ollama-mesh/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
