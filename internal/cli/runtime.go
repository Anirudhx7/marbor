package cli

import (
	"fmt"
	"io"
)

// runRuntimeAction implements `marbor runtime start|stop|restart <node>` -
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

// runRuntimeLogs implements `marbor runtime logs <node> [--lines=N]` - POST
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

// runRuntimeDrain implements `marbor runtime drain <node> [--reason=X]` - POST
// /admin/nodes/{name}/drain. Marbor-internal routing state (never sent to the
// Marbor Agent) - same exit-code taxonomy as runRuntimeAction.
func runRuntimeDrain(flags *globalFlags, node, reason string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	result, err := client.DrainNode(node, reason)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: draining\n", node)
	return ExitOK
}

// runRuntimeUndrain implements `marbor runtime undrain <node>` - DELETE
// /admin/nodes/{name}/drain.
func runRuntimeUndrain(flags *globalFlags, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	result, err := client.UndrainNode(node)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: undrained\n", node)
	return ExitOK
}

// runRuntimeHealth implements `marbor runtime health <node>` - GET
// /admin/nodes/{name}/health-check, capability "runtime.health_check" - an
// on-demand active liveness probe (as opposed to the passive, poll-cycle
// health already shown on `marbor nodes`). A populated result with ok=false is
// a successful probe reporting a down runtime, not a CLI failure - it still
// exits ExitOK, matching the UI's checkNodeHealth, which renders the result
// rather than treating it as an error.
func runRuntimeHealth(flags *globalFlags, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	result, err := client.HealthCheck(node)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}

	if result.OK {
		fmt.Fprintf(stdout, "%s: ok (%dms)\n", node, result.LatencyMs)
	} else {
		fmt.Fprintf(stdout, "%s: unhealthy - %s\n", node, result.Error)
	}
	return ExitOK
}
