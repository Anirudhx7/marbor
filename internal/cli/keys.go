package cli

import (
	"fmt"
	"io"
)

// printKeyUsage documents the "key" command group. Only set-local-only ships
// here (P66) - broader key add/list/patch/revoke CLI parity is a separate,
// pre-existing gap this item does not widen.
func printKeyUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: ollama-mesh key <action> [args] [flags]\n\nActions:\n")
	renderTable(w, "  ", [][2]string{
		{"set-local-only <name> <true|false>", "block (or re-allow) cloud fallback for one API key"},
	})
	fmt.Fprint(w, "\nFlags:\n")
	renderTable(w, "  ", authFlagsRows)
}

// runKeySetLocalOnly implements `mesh key set-local-only <name> <true|false>`
// - PATCH /admin/v1/keys/{name} with local_only.
func runKeySetLocalOnly(flags *globalFlags, name, value string, stdout, stderr io.Writer) int {
	var localOnly bool
	switch value {
	case "true":
		localOnly = true
	case "false":
		localOnly = false
	default:
		fmt.Fprintf(stderr, "invalid value %q for local_only (want true or false)\n", value)
		return ExitUserError
	}

	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.PatchKeyLocalOnly(name, localOnly); err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintf(stdout, "key %q local_only=%v\n", name, localOnly)
	return ExitOK
}

// runSpill implements `mesh spill` - GET /admin/v1/spill, session-authed.
func runSpill(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	rows, err := client.SpillCounters()
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, rows); handled {
		return code
	}

	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "KEY\tSERVED BY\tREQUESTS")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", row.KeyName, row.ServedBy, row.Requests)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
