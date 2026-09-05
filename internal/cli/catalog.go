package cli

// catalog.go - `marbor catalog`, `marbor models search`, `marbor models repo`
// (GET /admin/models/catalog, /admin/models/search, /admin/models/repo had
// full UI coverage in ModelAdvisor.tsx but no CLI). All three print the raw server JSON rather
// than a hand-mirrored type - see Client.ModelCatalog's doc comment.

import (
	"fmt"
	"io"
)

func printCatalogUsage(w io.Writer) { writeHelp(w, findCommand(root(), "catalog")) }

// runCatalog implements `marbor catalog`.
func runCatalog(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.ModelCatalog()
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runModelsSearch implements `marbor models search [--q --runtime --sort]`.
func runModelsSearch(flags *globalFlags, query, runtime, sort string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.ModelSearch(ModelSearchOpts{Query: query, Runtime: runtime, Sort: sort})
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runModelsRepo implements `marbor models repo <owner/name> [--node
// --runtime --ctx]`.
func runModelsRepo(flags *globalFlags, id, node, runtime string, ctxLen int, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.ModelRepo(ModelRepoOpts{ID: id, Node: node, Runtime: runtime, CtxLen: ctxLen})
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}
