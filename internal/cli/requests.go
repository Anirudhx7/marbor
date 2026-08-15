package cli

// requests.go - P41 per-request routing explainability CLI surface. First
// command touching the requests/audit surface (see admin's GET /admin/v1/
// requests and /admin/v1/audit, previously unreached from the CLI).

import (
	"fmt"
	"io"
)

func printRequestsUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: ollama-mesh requests explain <request-id> [flags]\n\nActions:\n")
	renderTable(w, "  ", [][2]string{
		{"explain <request-id>", "show why the router picked the node it did for one request"},
	})
	fmt.Fprint(w, "\nFlags:\n")
	renderTable(w, "  ", authFlagsRows)
}

func runRequestsExplain(flags *globalFlags, requestID string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	decision, err := client.ExplainRequest(requestID)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, decision); handled {
		return code
	}

	fmt.Fprintf(stdout, "Node:   %s\n", decision.Node)
	fmt.Fprintf(stdout, "Reason: %s\n", decision.Reason)
	if decision.Detail != "" {
		fmt.Fprintf(stdout, "Detail: %s\n", decision.Detail)
	}
	if decision.AffinityLost {
		fmt.Fprintln(stdout, "Note:   session affinity existed for this request but did not validate")
	}
	if len(decision.Components) == 0 {
		return ExitOK
	}
	fmt.Fprintf(stdout, "Score:  %.2f\n\n", decision.Score)
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "COMPONENT\tRAW\tWEIGHT\tVALUE")
	for _, c := range decision.Components {
		fmt.Fprintf(tw, "%s\t%.3f\t%.1f\t%.2f\n", c.Name, c.Raw, c.Weight, c.Value)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
