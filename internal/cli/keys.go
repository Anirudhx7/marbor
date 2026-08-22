package cli

import (
	"fmt"
	"io"
)

// printKeyUsage documents the "key" command group. Only set-local-only and
// set-allow-local-degradation ship here (P66, P67) - broader key
// add/list/patch/revoke CLI parity is a separate, pre-existing gap this item
// does not widen.
//
// This is a thin wrapper over the registry-backed writeHelp (help.go) - see
// the P83+ CLI hardening plan, migration step 4.
func printKeyUsage(w io.Writer) { writeHelp(w, findCommand(root(), "key")) }

// runKeySetLocalOnly implements `marbor key set-local-only <name> <true|false>`
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

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "key": name, "local_only": localOnly,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "key %q local_only=%v\n", name, localOnly)
	return ExitOK
}

// runKeySetAllowLocalDegradation implements
// `marbor key set-allow-local-degradation <name> <true|false>` - PATCH
// /admin/v1/keys/{name} with allow_local_degradation.
func runKeySetAllowLocalDegradation(flags *globalFlags, name, value string, stdout, stderr io.Writer) int {
	var allow bool
	switch value {
	case "true":
		allow = true
	case "false":
		allow = false
	default:
		fmt.Fprintf(stderr, "invalid value %q for allow_local_degradation (want true or false)\n", value)
		return ExitUserError
	}

	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.PatchKeyAllowLocalDegradation(name, allow); err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "key": name, "allow_local_degradation": allow,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "key %q allow_local_degradation=%v\n", name, allow)
	return ExitOK
}

// runSpill implements `marbor spill` - GET /admin/v1/spill, session-authed.
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
