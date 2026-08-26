package cli

import (
	"fmt"
	"io"
	"strings"
)

// activityKind maps a system_audit action to its fleet-operations kind bucket.
// Taxonomy locked per P389 tweaks, mirrors ui/src/lib/activityKind.ts.
func activityKind(action string) string {
	switch action {
	case "drain_node", "undrain_node", "set_node_prewarm":
		return "drain"
	case "enable_marbor_agent", "disable_marbor_agent", "regenerate_marbor_agent_token", "enroll_marbor_agent":
		return "agent"
	case "runtime_start", "runtime_stop", "runtime_restart", "accept_node_control", "clear_node_control":
		return "runtime"
	case "add_node", "update_node", "remove_node", "patch_node":
		return "node"
	case "unload_model", "set_node_warmup", "set_pinned_models", "pull_model", "pull_model_load_failed", "pull_model_cancel", "delete_model":
		return "warmup"
	}
	if strings.HasPrefix(action, "drain_") || strings.HasPrefix(action, "undrain") || action == "set_node_prewarm" {
		return "drain"
	}
	if strings.Contains(action, "marbor_agent") || strings.Contains(action, "_agent") {
		return "agent"
	}
	if strings.HasPrefix(action, "runtime_") || strings.Contains(action, "_control") {
		return "runtime"
	}
	if strings.HasPrefix(action, "add_node") || strings.HasPrefix(action, "remove_node") || strings.HasPrefix(action, "patch_node") || action == "update_node" {
		return "node"
	}
	if strings.HasPrefix(action, "unload") || strings.Contains(action, "warmup") || strings.Contains(action, "pinned") || strings.HasPrefix(action, "pull_model") || action == "delete_model" {
		return "warmup"
	}
	return "config"
}

func runActivity(flags *globalFlags, limit int, kind string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	validKinds := map[string]bool{"": true, "all": true, "drain": true, "agent": true, "runtime": true, "node": true, "warmup": true, "predictive": true, "config": true}
	if !validKinds[kind] {
		fmt.Fprintf(stderr, "error: unknown kind %q (want drain, agent, runtime, node, warmup, predictive, config, or all)\n", kind)
		return ExitUserError
	}
	if kind == "all" {
		kind = ""
	}

	entries, err := client.SystemAudit(limit)
	if err != nil {
		return reportError(err, stderr)
	}
	decisions, err := client.PredictiveDecisions()
	if err != nil {
		if flags.jsonOutput {
			return reportError(err, stderr)
		}
		fmt.Fprintf(stderr, "warning: could not fetch predictive decisions: %v\n", err)
		decisions = nil
	}

	var filtered []SystemAuditEntry
	for _, e := range entries {
		k := activityKind(e.Action)
		if kind != "" && kind != "predictive" && k != kind {
			continue
		}
		if kind == "predictive" {
			continue
		}
		filtered = append(filtered, e)
	}
	var filteredDecisions []PredictiveDecision
	if kind == "" || kind == "predictive" {
		filteredDecisions = decisions
		if len(filteredDecisions) > 10 {
			filteredDecisions = filteredDecisions[len(filteredDecisions)-10:]
		}
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"events":               filtered,
		"predictive_decisions": filteredDecisions,
	}); handled {
		return code
	}

	tw := newTabWriter(stdout)
	if len(filteredDecisions) > 0 {
		fmt.Fprintln(tw, "PREDICTIVE DECISIONS\t\t\t")
		fmt.Fprintln(tw, "WHEN\tPREDICTED MODEL\tNODE\tRESULT")
		for _, d := range filteredDecisions {
			result := "skipped"
			if d.WasAlreadyWarm {
				result = "already warm"
			} else if d.WarmupTriggered {
				result = "warmup triggered"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Timestamp, d.PredictedModel, d.Node, result)
		}
		fmt.Fprintln(tw, "")
	}
	if len(filtered) == 0 {
		if kind == "predictive" {
			if err := tw.Flush(); err != nil {
				fmt.Fprintln(stderr, err)
				return ExitServerError
			}
			return ExitOK
		}
		fmt.Fprintln(stdout, "No activity events matching filter.")
		if err := tw.Flush(); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitServerError
		}
		return ExitOK
	}
	fmt.Fprintln(tw, "TIME\tKIND\tACTION\tTARGET\tWHO")
	for _, e := range filtered {
		k := activityKind(e.Action)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Time, k, e.Action, e.Target, e.Username)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
