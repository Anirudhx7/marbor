package cli

import (
	"errors"
	"fmt"
	"io"
)

// runRuntimeAction implements `marbor runtime start|stop|restart <node>` -
// POST /admin/nodes/{name}/runtime/{action}, the CLI's first mutating
// command. Exit codes: ExitUserError for a bad/missing node
// argument or a user-actionable rejection (unconfigured node, unknown node,
// missing capability), ExitServerError for an agent/network failure,
// ExitAuthError for a 401/403 - never a silent ExitOK for an action that
// didn't happen (the same honest-data principle extended to CLI exit codes).
//
// A stop or restart on a multi-host replica head, worker or unresolved member
// is rejected by the server unless acknowledgeReplica is set (the
// --acknowledge-replica flag); the rejection message is printed as the error
// and exits ExitUserError. When the action does run on a replica member, the
// replica warning is printed to stderr so the effect is on record.
func runRuntimeAction(flags *globalFlags, action, node string, acknowledgeReplica, force, dryRun bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	// Only a restart can leave a model without a server: start and stop are
	// not checked.
	var gate soleGate
	if soleAction(action) == soleActionRestart {
		if gate = gateSoleReplica(client, flags, soleRequest{action: soleAction(action), node: node, force: force, dryRun: dryRun}, stdout, stderr); gate.done {
			return gate.code
		}
	}

	out, err := client.RuntimeAction(node, action, acknowledgeReplica, gate.sendForce)
	if errors.Is(err, ErrReplicaDetailUnavailable) {
		// The action ran; only the replica detail could not be read.
		fmt.Fprintf(stderr, "note: %s\n", err)
	} else if err != nil {
		return reportError(err, stderr)
	}
	if out == nil {
		out = &RuntimeActionResult{}
	}
	printReplicaWarning(stderr, out.Replica)

	result := map[string]interface{}{"ok": true, "node": node, "action": action}
	if out.Replica != nil {
		result["replica"] = out.Replica
	}
	if sole := soleReplicaForOutput(out.SoleReplica, gate.impact, gate.sendForce); sole != nil {
		result["sole_replica"] = sole
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: runtime %s ok\n", node, action)
	return ExitOK
}

// acknowledgeReplicaFlag is the opt-in that lets "runtime stop" and "runtime
// restart" run on a multi-host replica head, worker or unresolved member.
var acknowledgeReplicaFlag = FlagSpec{
	Name:  "acknowledge-replica",
	Kind:  FlagBool,
	Usage: "proceed when the node is part of a multi-host replica (stopping any member breaks the replica)",
}

// printReplicaWarning writes the server's replica warning to stderr, so it
// never pollutes piped stdout or --json output. A nil replica or an empty
// warning prints nothing.
func printReplicaWarning(stderr io.Writer, replica *ReplicaInfo) {
	if replica == nil || replica.Warning == "" {
		return
	}
	fmt.Fprintf(stderr, "warning: %s\n", replica.Warning)
}

// runRuntimeLogs implements `marbor runtime logs <node> [--lines=N]` - POST
// /admin/nodes/{name}/runtime/logs?lines=N. A pure read, same exit-code
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

// runRuntimeDrain implements `marbor runtime drain <node> [--reason=X]
// [--grace-period=N]` - POST /admin/nodes/{name}/drain. Marbor-internal
// routing state (never sent to the Marbor Agent) - same exit-code taxonomy
// as runRuntimeAction. gracePeriod < 0 means the flag was not passed
// (infinite drain, today's frozen default) - matches Client.DrainNode.
func runRuntimeDrain(flags *globalFlags, node, reason string, gracePeriod int, force, dryRun bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	gate := gateSoleReplica(client, flags, soleRequest{action: soleActionDrain, node: node, force: force, dryRun: dryRun}, stdout, stderr)
	if gate.done {
		return gate.code
	}

	result, err := client.DrainNode(node, reason, gracePeriod, gate.sendForce)
	if err != nil {
		return reportError(err, stderr)
	}

	printReplicaWarning(stderr, result.Replica)
	result.SoleReplica = soleReplicaForOutput(result.SoleReplica, gate.impact, gate.sendForce)

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: draining", node)
	if result.Reason != "" {
		fmt.Fprintf(stdout, " (reason: %s)", result.Reason)
	}
	if result.GracePeriodSeconds > 0 {
		fmt.Fprintf(stdout, " grace period: %ds", result.GracePeriodSeconds)
	} else {
		fmt.Fprint(stdout, " grace period: infinite")
	}
	fmt.Fprintln(stdout)
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
