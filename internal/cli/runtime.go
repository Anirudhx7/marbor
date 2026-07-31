package cli

import (
	"fmt"
	"io"
)

// runRuntimeAction implements `mesh runtime start|stop|restart <node>` -
// POST /admin/nodes/{name}/runtime/{action}, the CLI's first mutating
// command (P43 Step 3). Exit codes: ExitUserError for a bad/missing node
// argument or a user-actionable rejection (unconfigured node, unknown node,
// missing capability), ExitServerError for an agent/network failure,
// ExitAuthError for a 401/403 - never a silent ExitOK for an action that
// didn't happen (R1 extended to CLI exit codes).
func runRuntimeAction(flags *globalFlags, action, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	if err := client.RuntimeAction(node, action); err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": node, "action": action,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: runtime %s ok\n", node, action)
	return ExitOK
}

// runRuntimeLogs implements `mesh runtime logs <node> [--lines=N]` - POST
// /admin/nodes/{name}/runtime/logs?lines=N (P58). A pure read, same exit-code
// taxonomy as runRuntimeAction. Lines print raw to stdout (no prefix) so
// output pipes cleanly into grep/less.
func runRuntimeLogs(flags *globalFlags, node string, lines int, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	logLines, err := client.RuntimeLogs(node, lines)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"node": node, "lines": logLines,
	}); handled {
		return code
	}

	for _, line := range logLines {
		fmt.Fprintln(stdout, line)
	}
	return ExitOK
}
