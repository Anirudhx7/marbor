package cli

// diagnostics.go - pull triage (`marbor pulls`, `marbor models pull-progress/cancel-pull`),
// predictive/warmup/system-info/config-reload helpers (`marbor warmup
// status/predictive/ping`, `marbor system-info`, `marbor predictive
// decisions`, `marbor config reload`), and `marbor users pending-count`.
// All had full UI coverage but no CLI.

import (
	"fmt"
	"io"
)

func printWarmupUsage(w io.Writer) { writeHelp(w, findCommand(root(), "warmup")) }

// runPulls implements `marbor pulls` - GET /admin/pulls, every active pull
// job across the fleet.
func runPulls(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	pulls, err := client.ActivePulls()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, pulls); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NODE\tMODEL\tMETHOD\tSTATUS\tBYTES\tSTARTED")
	for _, p := range pulls {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n", p.Node, p.Model, p.Method, p.Status, p.BytesCompleted, p.BytesTotal, p.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runModelsPullProgress implements `marbor models pull-progress <node>
// <model>` - a single point-in-time snapshot from the active-pulls list
// (a point-in-time read), not a live SSE follow (one Admin API request
// per CLI command).
func runModelsPullProgress(flags *globalFlags, node, model string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	pulls, err := client.ActivePulls()
	if err != nil {
		return reportError(err, stderr)
	}
	for _, p := range pulls {
		if p.Node == node && p.Model == model {
			if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, p); handled {
				return code
			}
			fmt.Fprintf(stdout, "status=%s bytes=%d/%d method=%s\n", p.Status, p.BytesCompleted, p.BytesTotal, p.Method)
			return ExitOK
		}
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"found": false}); handled {
		return code
	}
	fmt.Fprintf(stdout, "no active pull for %q on %q\n", model, node)
	return ExitOK
}

// runModelsCancelPull implements `marbor models cancel-pull <node> <model>`.
func runModelsCancelPull(flags *globalFlags, node, model string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	cancelled, err := client.CancelPull(node, model)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "cancelled": cancelled}); handled {
		return code
	}
	if cancelled {
		fmt.Fprintf(stdout, "pull of %q on %q cancelled\n", model, node)
	} else {
		fmt.Fprintf(stdout, "no active pull of %q on %q to cancel\n", model, node)
	}
	return ExitOK
}

// runWarmupStatus implements `marbor warmup status`.
func runWarmupStatus(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	s, err := client.GetWarmupStatus()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, s); handled {
		return code
	}
	fmt.Fprintf(stdout, "enabled=%v interval_ms=%d keep_alive=%s models=%v predictive_engine_enabled=%v\n",
		s.Enabled, s.IntervalMs, s.KeepAlive, s.Models, s.PredictiveEngineEnabled)
	return ExitOK
}

// runWarmupPredictiveSet implements `marbor warmup predictive set --enabled
// true|false`.
func runWarmupPredictiveSet(flags *globalFlags, enabled bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.SetPredictiveEngine(enabled); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"predictive_engine_enabled": enabled}); handled {
		return code
	}
	fmt.Fprintf(stdout, "predictive engine enabled=%v\n", enabled)
	return ExitOK
}

// runWarmupPing implements `marbor warmup ping`.
func runWarmupPing(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.WarmupPing(); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"status": "triggered"}); handled {
		return code
	}
	fmt.Fprintln(stdout, "warmup cycle triggered")
	return ExitOK
}

// runPredictiveDecisions implements `marbor predictive decisions` - exposes
// the Client.PredictiveDecisions method that existed with no command
// wired to it before this item.
func runPredictiveDecisions(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	decisions, err := client.PredictiveDecisions()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, decisions); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "TIME\tPREDICTED\tTRIGGER\tNODE\tALREADY WARM\tTRIGGERED")
	for _, d := range decisions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%v\n", d.Timestamp, d.PredictedModel, d.TriggerModel, d.Node, d.WasAlreadyWarm, d.WarmupTriggered)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runSystemInfo implements `marbor system-info`. Prints the raw JSON
// response - see Client.ModelCatalog's doc comment for why.
func runSystemInfo(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.SystemInfo()
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runConfigReload implements `marbor config reload`.
func runConfigReload(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	result, err := client.ConfigReload()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}
	fmt.Fprintf(stdout, "reloaded: auth keys=%d, nodes +%d/-%d, cloud providers=%d\n", result.AuthKeys, result.NodesAdded, result.NodesRemoved, result.CloudProviders)
	return ExitOK
}

// runUsersPendingCount implements `marbor users pending-count`.
func runUsersPendingCount(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	count, err := client.PendingUserCount()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"count": count}); handled {
		return code
	}
	fmt.Fprintln(stdout, count)
	return ExitOK
}
