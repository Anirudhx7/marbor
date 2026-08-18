// Command gen-docs generates the CLI reference documentation - man pages
// (docs/man/*.1), the Markdown reference (docs/cli.md), and the README CLI
// table - from the live internal/cli command registry (cli.Root()). It is a
// standalone package, never imported by the shipped binary's main.go or any
// internal/ package: cmd/gen-docs -> internal/cli is a one-directional
// dependency, so the registry never depends back on this generator. Stdlib
// only - it is built by `go build ./...` like every other package, so any
// non-stdlib import here would violate the project's zero-dependency law
// transitively.
//
// Determinism (P83+ plan Implementation section 6): running this twice
// produces byte-identical output. Every walk below iterates cli.Command.Sub
// in declared order - never map iteration - and the only "date" in the
// output is docsDate, a hand-bumped constant, never time.Now(). The man
// page's version field uses cli.Version, which defaults to "dev" and is
// never overridden here (gen-docs never calls main.go's ldflags-injected
// assignment) - that default is itself deterministic, which is the property
// that matters for the CI drift check; it does not need to match the
// binary's real release version to do its job.
//
// Usage: go run ./cmd/gen-docs   (or `make man` / `make docs`)
package main

import (
	"fmt"
	"os"

	"github.com/ollama-mesh/ollama-mesh/internal/cli"
)

// docsDate is the man page ".TH" date field. Bumped by hand whenever the
// docs are regenerated for a release - never time.Now(), which would make
// every regeneration produce a diff even when nothing about the CLI changed,
// defeating the whole point of the CI drift check.
const docsDate = "2026-08-17"

func main() {
	root := cli.Root()

	if err := generateManPages(root); err != nil {
		fmt.Fprintln(os.Stderr, "gen-docs: man pages:", err)
		os.Exit(1)
	}
	if err := generateMarkdown(root); err != nil {
		fmt.Fprintln(os.Stderr, "gen-docs: docs/cli.md:", err)
		os.Exit(1)
	}
	if err := generateReadmeTable(root); err != nil {
		fmt.Fprintln(os.Stderr, "gen-docs: README CLI table:", err)
		os.Exit(1)
	}

	fmt.Println("gen-docs: docs/man/*.1, docs/cli.md, and the README CLI table are up to date.")
}
