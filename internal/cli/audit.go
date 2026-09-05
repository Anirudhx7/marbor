package cli

// audit.go - `marbor audit`, the request audit log's own CLI surface
// (GET /admin/audit, internal/audit.Entry), distinct from `marbor activity`
// which covers GET /admin/system-audit (operator actions like drain/agent/
// runtime). Added because the UI's Requests.tsx audit view had no CLI
// equivalent.

import (
	"fmt"
	"io"
)

func printAuditUsage(w io.Writer) { writeHelp(w, findCommand(root(), "audit")) }

// runAudit implements `marbor audit [--limit] [--model] [--key] [--node]
// [--status] [--cloud] [--since] [--until] [--json]`.
func runAudit(flags *globalFlags, limit int, model, key, node, status, cloud, since, until string, stdout, stderr io.Writer) int {
	f := AuditFilter{
		Limit:  limit,
		Model:  model,
		Key:    key,
		Node:   node,
		Status: status,
		Since:  since,
		Until:  until,
	}
	if cloud != "" {
		b := cloud == "true"
		f.Cloud = &b
	}

	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	result, err := client.AuditQuery(f)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "TIME\tREQUEST ID\tKEY\tMODEL\tNODE\tSTATUS\tLATENCY MS\tCLOUD")
	for _, e := range result.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			e.Time.Format("2006-01-02T15:04:05Z07:00"), e.RequestID, e.KeyName, e.Model, e.Node, e.Status, e.LatencyMs, yesNo(e.Cloud))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	if result.Truncated {
		fmt.Fprintf(stdout, "(showing %d of possibly more - raise --limit for older entries)\n", result.Total)
	}
	return ExitOK
}
