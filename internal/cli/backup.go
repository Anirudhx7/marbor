package cli

// backup.go - `marbor backup now/list/restore/upload`, `marbor analytics
// show/export`, `marbor savings`, and `marbor metrics summary` (from the
// three-surface-parity audit: all 8 endpoints had full UI coverage in
// Settings.tsx/Analytics.tsx/Dashboard.tsx but no CLI).

import (
	"fmt"
	"io"
	"os"
)

func printBackupUsage(w io.Writer)    { writeHelp(w, findCommand(root(), "backup")) }
func printAnalyticsUsage(w io.Writer) { writeHelp(w, findCommand(root(), "analytics")) }

// runBackupNow implements `marbor backup now [--output path]`. Downloads
// the on-demand backup to a local file (default: the server-suggested
// filename in the current directory).
func runBackupNow(flags *globalFlags, output string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	filename, data, err := client.BackupNow()
	if err != nil {
		return reportError(err, stderr)
	}
	if output == "" {
		output = filename
	}
	if err := os.WriteFile(output, data, 0o600); err != nil {
		fmt.Fprintf(stderr, "error: writing %s: %v\n", output, err)
		return ExitServerError
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": output, "bytes": len(data)}); handled {
		return code
	}
	fmt.Fprintf(stdout, "backup saved to %s (%d bytes)\n", output, len(data))
	return ExitOK
}

// runBackupList implements `marbor backup list`.
func runBackupList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	backups, err := client.ListBackups()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, backups); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tSIZE\tMODIFIED")
	for _, b := range backups {
		fmt.Fprintf(tw, "%s\t%d bytes\t%s\n", b.Name, b.SizeBytes, b.ModifiedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runBackupRestore implements `marbor backup restore <filename> [--yes]`.
// Destructive (overwrites mesh.db and restarts marbor): requires
// --yes or an interactive TTY confirmation, matching the "key revoke"
// pattern.
func runBackupRestore(flags *globalFlags, filename string, yes bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("restore backup (marbor will restart)", filename, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.RestoreBackup(filename); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": filename, "restarting": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "restore from %q requested - marbor is restarting\n", filename)
	return ExitOK
}

// runBackupUpload implements `marbor backup upload --file <local-path>`.
func runBackupUpload(flags *globalFlags, path string, stdout, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: opening %s: %v\n", path, err)
		return ExitUserError
	}
	defer f.Close()
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.UploadBackup(path, f); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": path, "uploaded": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "%q uploaded as a restorable backup\n", path)
	return ExitOK
}

// runAnalyticsShow implements `marbor analytics show`. Prints the raw JSON
// response - see Client.ModelCatalog's doc comment for why (large,
// purely-informational dashboard aggregate).
func runAnalyticsShow(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.AnalyticsSummary()
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runAnalyticsExport implements `marbor analytics export [--type hourly|models]
// [--format json|csv] [--output path]`.
func runAnalyticsExport(flags *globalFlags, exportType, format, output string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	filename, data, err := client.AnalyticsExport(exportType, format)
	if err != nil {
		return reportError(err, stderr)
	}
	if output == "" {
		output = filename
	}
	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "error: writing %s: %v\n", output, err)
		return ExitServerError
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": output, "bytes": len(data)}); handled {
		return code
	}
	fmt.Fprintf(stdout, "analytics export saved to %s (%d bytes)\n", output, len(data))
	return ExitOK
}

// runSavings implements `marbor savings`.
func runSavings(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	s, err := client.Savings()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, s); handled {
		return code
	}
	savedStr, spentStr := "-", "-"
	if s.SavedUSD != nil {
		savedStr = fmt.Sprintf("$%.2f", *s.SavedUSD)
	}
	if s.CloudSpentUSD != nil {
		spentStr = fmt.Sprintf("$%.2f", *s.CloudSpentUSD)
	}
	fmt.Fprintf(stdout, "local requests: %d, cloud requests: %d, saved: %s, cloud spent: %s (since %s)\n",
		s.LocalRequests, s.CloudRequests, savedStr, spentStr, s.Since)
	return ExitOK
}

// runMetricsSummary implements `marbor metrics summary`.
func runMetricsSummary(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	s, err := client.MetricsSummary()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, s); handled {
		return code
	}
	fmt.Fprintf(stdout, "nodes: %d/%d online (%d draining), active requests: %d, queue depth: %d, avg latency: %.1fms, tokens/min: %d, warm hit ratio: %.1f%%\n",
		s.NodesOnline, s.TotalNodes, s.NodesDraining, s.ActiveRequests, s.QueueDepth, s.AvgLatency, s.TokensPerMin, s.WarmHitRatio*100)
	return ExitOK
}
