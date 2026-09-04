package cli

// cloud.go - `marbor cloud providers list/add/update/delete/reorder/test`
// and `marbor cloud budget-status` (P-A2-05, A2 three-surface-parity audit:
// all 7 cloud-provider/budget endpoints had full UI coverage in Settings.tsx
// but no CLI). R8: the provider's API key is write-only - never printed back
// by "list", and an "update" with no --api-key leaves the stored key
// unchanged server-side (handleUpdateCloudProvider).

import (
	"fmt"
	"io"
	"strconv"
)

func printCloudUsage(w io.Writer) { writeHelp(w, findCommand(root(), "cloud")) }

// parseCostPer1K parses the --cost-per-1k flag value, treating "" as 0
// (unset), matching the loose-optional-float convention keys.go already
// uses for --daily-usd-cap/--monthly-usd-cap.
func parseCostPer1K(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// runCloudProvidersList implements `marbor cloud providers list`.
func runCloudProvidersList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	providers, err := client.CloudProviders()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, providers); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tPROVIDER\tBASE URL\tDEFAULT MODEL\t$/1K TOK\tENABLED\tPRIORITY")
	for _, p := range providers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.4f\t%v\t%d\n", p.Name, p.Provider, p.BaseURL, p.DefaultModel, p.CostPer1KTokens, p.Enabled, p.Priority)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runCloudProvidersAdd implements `marbor cloud providers add --name
// --provider --base-url [--api-key --default-model --cost-per-1k
// --priority --enabled]`.
func runCloudProvidersAdd(flags *globalFlags, name, provider, baseURL, apiKey, defaultModel, costPer1KRaw string, priority int, enabled bool, stdout, stderr io.Writer) int {
	costPer1K, err := parseCostPer1K(costPer1KRaw)
	if err != nil {
		fmt.Fprintf(stderr, "invalid --cost-per-1k %q: %v\n", costPer1KRaw, err)
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.AddCloudProvider(CloudProviderRequest{
		Name: name, Provider: provider, BaseURL: baseURL, APIKey: apiKey,
		DefaultModel: defaultModel, CostPer1KTokens: costPer1K, Priority: priority, Enabled: enabled,
	}); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "name": name}); handled {
		return code
	}
	fmt.Fprintf(stdout, "cloud provider %q added\n", name)
	return ExitOK
}

// runCloudProvidersUpdate implements `marbor cloud providers update <name>
// --provider --base-url [--api-key --default-model --cost-per-1k --priority
// --enabled]`. Leaving --api-key unset sends "" so the server preserves the
// currently stored key (R8) - see handleUpdateCloudProvider.
func runCloudProvidersUpdate(flags *globalFlags, name, provider, baseURL, apiKey, defaultModel, costPer1KRaw string, priority int, enabled bool, stdout, stderr io.Writer) int {
	costPer1K, err := parseCostPer1K(costPer1KRaw)
	if err != nil {
		fmt.Fprintf(stderr, "invalid --cost-per-1k %q: %v\n", costPer1KRaw, err)
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.UpdateCloudProvider(name, CloudProviderRequest{
		Name: name, Provider: provider, BaseURL: baseURL, APIKey: apiKey,
		DefaultModel: defaultModel, CostPer1KTokens: costPer1K, Priority: priority, Enabled: enabled,
	}); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "name": name}); handled {
		return code
	}
	fmt.Fprintf(stdout, "cloud provider %q updated\n", name)
	return ExitOK
}

// runCloudProvidersDelete implements `marbor cloud providers delete <name>
// [--yes]`. Destructive per R10: requires --yes or an interactive TTY
// confirmation, matching the "key revoke"/"nodes remove" pattern.
func runCloudProvidersDelete(flags *globalFlags, name string, yes bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("delete cloud provider", name, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DeleteCloudProvider(name); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "name": name, "deleted": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "cloud provider %q deleted\n", name)
	return ExitOK
}

// runCloudProvidersReorder implements `marbor cloud providers reorder
// <name1,name2,...>`.
func runCloudProvidersReorder(flags *globalFlags, order string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	names := parseCommaList(order)
	if err := client.ReorderCloudProviders(names); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "order": names}); handled {
		return code
	}
	fmt.Fprintf(stdout, "cloud provider priority set: %v\n", names)
	return ExitOK
}

// runCloudProvidersTest implements `marbor cloud providers test --provider
// --base-url --api-key`. The key is sent for this one-shot probe only -
// never persisted or echoed back (R8).
func runCloudProvidersTest(flags *globalFlags, provider, baseURL, apiKey string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.TestCloudProvider(provider, baseURL, apiKey); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true}); handled {
		return code
	}
	fmt.Fprintln(stdout, "cloud provider credentials OK")
	return ExitOK
}

// runCloudBudgetStatus implements `marbor cloud budget-status`.
func runCloudBudgetStatus(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	status, err := client.CloudBudgetStatus()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, status); handled {
		return code
	}
	fmt.Fprintf(stdout, "GLOBAL: daily $%.2f/%.2f (%.1f%%), monthly $%.2f/%.2f (%.1f%%), soft budget %.0f%%\n",
		status.Global.DailySpent, status.Global.DailyCap, status.Global.DailyPct*100,
		status.Global.MonthlySpent, status.Global.MonthlyCap, status.Global.MonthlyPct*100, status.SoftBudgetPct*100)
	if len(status.PerKey) == 0 {
		return ExitOK
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "KEY\tDAILY SPENT/CAP\tDAILY %\tMONTHLY SPENT/CAP\tMONTHLY %")
	for _, e := range status.PerKey {
		fmt.Fprintf(tw, "%s\t$%.2f/%.2f\t%.1f%%\t$%.2f/%.2f\t%.1f%%\n", e.Name, e.DailySpent, e.DailyCap, e.DailyPct*100, e.MonthlySpent, e.MonthlyCap, e.MonthlyPct*100)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
