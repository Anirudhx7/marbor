package cli

// routing.go - `marbor routing rules list/add/remove/toggle` and
// `marbor routing strategy get/set` (GET/POST/DELETE/PUT
// /admin/routing/rules and GET/PUT /admin/routing/strategy had full UI
// coverage in Routing.tsx but no CLI).

import (
	"fmt"
	"io"
)

func printRoutingUsage(w io.Writer) { writeHelp(w, findCommand(root(), "routing")) }

// runRoutingRulesList implements `marbor routing rules list`.
func runRoutingRulesList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	rules, err := client.RoutingRules()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, rules); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "ID\tPRIORITY\tCONDITION\tTARGET\tSTRATEGY\tENABLED")
	for _, r := range rules {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%v\n", r.ID, r.Priority, r.Condition, r.TargetNode, r.Strategy, r.Enabled)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runRoutingRulesAdd implements `marbor routing rules add --id --condition
// [--target --priority --strategy --enabled]`.
func runRoutingRulesAdd(flags *globalFlags, id, condition, target, strategy string, priority int, enabled bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	rule := RoutingRule{ID: id, Condition: condition, TargetNode: target, Strategy: strategy, Priority: priority, Enabled: enabled}
	if err := client.AddRoutingRule(rule); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, rule); handled {
		return code
	}
	fmt.Fprintf(stdout, "routing rule %q added\n", id)
	return ExitOK
}

// runRoutingRulesRemove implements `marbor routing rules remove <id>
// [--yes]`. Destructive (irreversible): requires --yes or an
// interactive TTY confirmation, matching the "key revoke"/"users delete"
// pattern (code review finding - this was originally missing).
func runRoutingRulesRemove(flags *globalFlags, id string, yes bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("remove routing rule", id, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.RemoveRoutingRule(id); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "id": id, "removed": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "routing rule %q removed\n", id)
	return ExitOK
}

// runRoutingRulesToggle implements `marbor routing rules toggle <id>`.
func runRoutingRulesToggle(flags *globalFlags, id string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.ToggleRoutingRule(id); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "id": id, "toggled": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "routing rule %q toggled\n", id)
	return ExitOK
}

// runRoutingStrategyGet implements `marbor routing strategy get`.
func runRoutingStrategyGet(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	strategy, err := client.RoutingStrategy()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"strategy": strategy}); handled {
		return code
	}
	fmt.Fprintln(stdout, strategy)
	return ExitOK
}

// runRoutingStrategySet implements `marbor routing strategy set <strategy>`.
func runRoutingStrategySet(flags *globalFlags, strategy string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.SetRoutingStrategy(strategy); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "strategy": strategy}); handled {
		return code
	}
	fmt.Fprintf(stdout, "routing strategy set to %q\n", strategy)
	return ExitOK
}
