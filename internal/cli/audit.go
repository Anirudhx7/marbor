package cli

// audit.go - `marbor audit`, the request audit log's own CLI surface
// (GET /admin/audit, internal/audit.Entry), distinct from `marbor activity`
// which covers GET /admin/system-audit (operator actions like drain/agent/
// runtime). Added because the UI's Requests.tsx audit view had no CLI
// equivalent.

import (
	"fmt"
	"io"
	"strconv"
)

// auditCursorArgs carries the --before-id / --after-id flag values and
// whether each was given, so 0 and negatives can be rejected rather than
// mistaken for "unset".
type auditCursorArgs struct {
	before, after       int
	beforeSet, afterSet bool
}

// validate returns a usage message when the cursor flags are misused, or "".
func (c auditCursorArgs) validate() string {
	switch {
	case c.beforeSet && c.afterSet:
		return "--before-id and --after-id cannot be used together"
	case c.beforeSet && c.before <= 0:
		return "--before-id must be a positive row id"
	case c.afterSet && c.after <= 0:
		return "--after-id must be a positive row id"
	}
	return ""
}

func printAuditUsage(w io.Writer) { writeHelp(w, findCommand(root(), "audit")) }

// runAudit implements `marbor audit [--limit] [--model] [--key] [--node]
// [--status] [--cloud] [--since] [--until] [--before-id] [--after-id]
// [--json]`.
func runAudit(flags *globalFlags, limit int, model, key, node, status, cloud, since, until string, cur auditCursorArgs, stdout, stderr io.Writer) int {
	if msg := cur.validate(); msg != "" {
		fmt.Fprintf(stderr, "error: %s\n", msg)
		return ExitUserError
	}
	// Cursor flags are plain ints, so row ids above 2^31-1 need a 64-bit build.
	f := AuditFilter{
		BeforeID: int64(cur.before),
		AfterID:  int64(cur.after),
		Limit:    limit,
		Model:    model,
		Key:      key,
		Node:     node,
		Status:   status,
		Since:    since,
		Until:    until,
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
	fmt.Fprintln(tw, "ID\tTIME\tREQUEST ID\tKEY\tMODEL\tNODE\tSTATUS\tLATENCY MS\tCLOUD")
	for _, e := range result.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			auditIDCell(e.ID), e.Time.Format("2006-01-02T15:04:05Z07:00"), e.RequestID, e.KeyName, e.Model, e.Node, e.Status, e.LatencyMs, yesNo(e.Cloud))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	if len(result.Entries) == 0 && (cur.beforeSet || cur.afterSet) {
		fmt.Fprintln(stdout, "(no entries: end of log)")
	}
	if result.NextBeforeID > 0 {
		fmt.Fprintf(stdout, "(more: --before-id %d for older entries)\n", result.NextBeforeID)
	}
	if result.PrevAfterID > 0 {
		fmt.Fprintf(stdout, "(newer: --after-id %d)\n", result.PrevAfterID)
	}
	if result.Truncated && result.NextBeforeID == 0 && result.PrevAfterID == 0 {
		fmt.Fprintf(stdout, "(showing %d of possibly more - raise --limit for older entries)\n", result.Total)
	}
	return ExitOK
}

// auditIDCell renders a row id, or "-" for entries from a server that does not
// report ids.
func auditIDCell(id int64) string {
	if id <= 0 {
		return "-"
	}
	return strconv.FormatInt(id, 10)
}
