package cli

// backup.go - `marbor backup now/list/download/restore/upload`, `marbor
// analytics show/export`, `marbor savings`, and `marbor metrics summary`: the
// command front ends for the matching Admin API endpoints (the transfer logic
// for backups lives in backup_client.go).

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

func printBackupUsage(w io.Writer)    { writeHelp(w, findCommand(root(), "backup")) }
func printAnalyticsUsage(w io.Writer) { writeHelp(w, findCommand(root(), "analytics")) }

// runBackupNow implements `marbor backup now [--output path] [--force]`.
// Downloads the on-demand backup to a local file (default: the server-suggested
// filename in the current directory). An existing file is never replaced
// unless --force is given.
func runBackupNow(flags *globalFlags, output string, force bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	path, n, err := client.BackupNow(output, force)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": path, "bytes": n}); handled {
		return code
	}
	fmt.Fprintf(stdout, "backup saved to %s (%d bytes, gzip)\n", path, n)
	return ExitOK
}

// runBackupDownload implements `marbor backup download <name> [--output path]
// [--force]`. Without --output the file is saved under the backup's own name,
// which must be a valid marbor backup name; an existing file is never replaced
// unless --force is given.
func runBackupDownload(flags *globalFlags, name, output string, force bool, stdout, stderr io.Writer) int {
	if output == "" {
		if !validBackupName(name) {
			return reportError(userErrorf("%q is not a marbor backup file name (expected marbor-backup-YYYYMMDD-HHMMSS.db.gz); use --output to choose a local file name", name), stderr)
		}
		output = name
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	path, n, err := client.DownloadBackup(name, output, force)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": path, "bytes": n}); handled {
		return code
	}
	fmt.Fprintf(stdout, "backup saved to %s (%d bytes)\n", path, n)
	return ExitOK
}

// runBackupList implements `marbor backup list`.
//
// With check set the command also exits ExitAttention when something needs a
// look: a failed or refused backup, a damaged or unverified file, a retention
// problem, leftover files, a failed restore. The listing is printed either way.
func runBackupList(flags *globalFlags, check bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	backups, status, err := client.ListBackups()
	if err != nil {
		return reportError(err, stderr)
	}
	problems := backupProblems(backups, status, time.Now())
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"backups": backups, "status": status, "problems": problems}); handled {
		if check && len(problems) > 0 {
			reportBackupProblems(stderr, problems)
			return ExitAttention
		}
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tSIZE\tTYPE\tSOURCE\tCHECK\tMODIFIED")
	for _, b := range backups {
		kind := "plain"
		if b.Compressed {
			kind = "gzip"
		}
		source := "server"
		if b.Uploaded {
			source = "uploaded"
		}
		check := b.Integrity
		if check == "" {
			check = "-"
		}
		fmt.Fprintf(tw, "%s\t%d bytes\t%s\t%s\t%s\t%s\n", b.Name, b.SizeBytes, kind, source, check, b.ModifiedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	printBackupStatus(stdout, status)
	if check && len(problems) > 0 {
		reportBackupProblems(stderr, problems)
		return ExitAttention
	}
	return ExitOK
}

// reportBackupProblems prints why --check exits 3, one reason per line.
func reportBackupProblems(stderr io.Writer, problems []string) {
	fmt.Fprintf(stderr, "%d backup problem(s) need attention:\n", len(problems))
	for _, p := range problems {
		fmt.Fprintln(stderr, "  - "+p)
	}
}

// overdueGrace is how long past the configured interval a scheduled backup may
// run late before --check calls it overdue: the scheduler checks on its own
// cadence and a backup itself takes time.
const overdueGrace = time.Hour

// longRunningBackup is how long a backup may run before --check calls it stuck.
const longRunningBackup = time.Hour

// backupProblems lists everything about the backups that needs the operator's
// attention, in plain words. Empty means all is well.
func backupProblems(backups []BackupFileInfo, s BackupStatus, now time.Time) []string {
	var out []string
	for label, o := range map[string]BackupOutcome{"scheduled backup": s.Scheduled, "manual backup": s.Manual} {
		switch o.Result {
		case "failed", "refused":
			out = append(out, label+" "+o.Result+": "+o.Error)
		case "cancelled":
			out = append(out, label+" was cancelled before it finished")
		}
		if o.Warning != "" {
			out = append(out, label+": "+o.Warning)
		}
	}
	if s.Manual.Note != "" {
		out = append(out, "manual backup: "+s.Manual.Note)
	}
	switch {
	case !s.DirExists:
		out = append(out, "the backup directory does not exist")
	case len(backups) == 0:
		out = append(out, "there are no backups")
	}
	if s.ScheduleEnabled {
		out = append(out, scheduleProblems(s, now)...)
	}
	if s.Notice != "" {
		out = append(out, s.Notice)
	}
	if s.DirScanError != "" {
		out = append(out, s.DirScanError)
	}
	if s.Skipped > 0 {
		out = append(out, fmt.Sprintf("%d entr(ies) in the backup directory could not be listed", s.Skipped))
	}
	if s.Running {
		if since, err := time.Parse(time.RFC3339, s.RunningSince); err == nil && now.Sub(since) > longRunningBackup {
			out = append(out, fmt.Sprintf("a backup has been running for %s", now.Sub(since).Round(time.Minute)))
		}
	}
	for _, b := range backups {
		switch b.Integrity {
		case "invalid":
			out = append(out, b.Name+" is damaged")
		case "unverified":
			out = append(out, b.Name+" could not be verified")
		}
	}
	for _, p := range []string{s.PruneError, s.UploadPruneError, s.DirWarning, s.StatusNotSaved} {
		if p != "" {
			out = append(out, p)
		}
	}
	if s.LeftoverFiles > 0 {
		out = append(out, fmt.Sprintf("%d leftover file(s) from an earlier run could not be removed", s.LeftoverFiles))
	}
	if r := s.LastRestore; r != nil {
		switch {
		case r.Result != "success":
			out = append(out, "the last restore "+r.Result+": "+r.Error)
		case r.Note != "":
			out = append(out, "the last restore succeeded, with a note: "+r.Note)
		}
	}
	sort.Strings(out)
	return out
}

// scheduleProblems says whether a scheduled backup that should be happening is
// not: none has ever succeeded, or the last success is older than the configured
// interval plus a grace period.
func scheduleProblems(s BackupStatus, now time.Time) []string {
	if s.Scheduled.SuccessAt == "" {
		return []string{"scheduled backups are on but none has succeeded yet"}
	}
	last, err := time.Parse(time.RFC3339, s.Scheduled.SuccessAt)
	if err != nil {
		return nil
	}
	interval := time.Duration(s.ScheduleIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if late := now.Sub(last) - interval; late > overdueGrace {
		return []string{fmt.Sprintf("the scheduled backup is overdue: the last success was %s ago and one is due every %s", now.Sub(last).Round(time.Minute), interval)}
	}
	return nil
}

// outcomeLine renders one source's last outcome, or "" when it never ran.
func outcomeLine(label string, o BackupOutcome) string {
	if o.Result == "" {
		return ""
	}
	line := label + ": " + o.Result
	if o.AttemptAt != "" {
		line += " at " + o.AttemptAt
	}
	if o.Error != "" {
		line += " (" + o.Error + ")"
	}
	if o.Result != "success" && o.SuccessAt != "" {
		line += "; last success " + o.SuccessAt
	}
	if o.Warning != "" {
		line += "; warning: " + o.Warning
	}
	if o.Note != "" {
		line += "; " + o.Note
	}
	return line
}

// printBackupStatus prints the scheduled and manual outcomes and every
// condition the operator should know about, one line each.
func printBackupStatus(w io.Writer, s BackupStatus) {
	for _, line := range []string{outcomeLine("scheduled backup", s.Scheduled), outcomeLine("manual backup", s.Manual)} {
		if line != "" {
			fmt.Fprintln(w, line)
		}
	}
	if s.LastRestore != nil {
		line := "last restore: " + s.LastRestore.Result
		if s.LastRestore.At != "" {
			line += " at " + s.LastRestore.At
		}
		if s.LastRestore.Error != "" {
			line += " (" + s.LastRestore.Error + ")"
		}
		if s.LastRestore.Note != "" {
			line += "; " + s.LastRestore.Note
		}
		switch {
		case s.LastRestore.Ref == "":
		case validRestoreRef(s.LastRestore.Ref):
			line += " [ref " + s.LastRestore.Ref + "]"
		default:
			line += " [ref not shown: unexpected format]"
		}
		fmt.Fprintln(w, line)
	}
	if s.Running {
		if s.RunningSince != "" {
			fmt.Fprintln(w, "a backup is running now (since "+s.RunningSince+")")
		} else {
			fmt.Fprintln(w, "a backup is running now")
		}
	}
	if !s.DirExists {
		fmt.Fprintln(w, "warning: the backup directory does not exist yet")
	}
	if s.DirWarning != "" {
		fmt.Fprintln(w, "warning: "+s.DirWarning)
	}
	if s.StatusNotSaved != "" {
		fmt.Fprintln(w, "warning: "+s.StatusNotSaved)
	}
	if s.PruneError != "" {
		fmt.Fprintln(w, "warning: "+s.PruneError)
	}
	if s.UploadPruneError != "" {
		fmt.Fprintln(w, "warning (uploaded files): "+s.UploadPruneError)
	}
	if s.Notice != "" {
		fmt.Fprintln(w, "note: "+s.Notice)
	}
	if s.DirScanError != "" {
		fmt.Fprintln(w, "warning: "+s.DirScanError)
	}
	if s.LeftoverFiles > 0 {
		fmt.Fprintf(w, "warning: %d leftover file(s) from an earlier run could not be removed\n", s.LeftoverFiles)
	}
	if s.Skipped > 0 {
		fmt.Fprintf(w, "note: %d entr(ies) in the backup directory could not be listed\n", s.Skipped)
	}
}

// restoreWaitTimeout is how long `backup restore --wait` waits for marbor to come
// back; restorePollInterval is how often it looks.
var (
	restoreWaitTimeout  = 5 * time.Minute
	restorePollInterval = time.Second
)

// runBackupRestore implements `marbor backup restore <filename> [--yes] [--wait]`.
// Destructive (replaces marbor.db with the backup and restarts marbor): requires
// --yes or an interactive TTY confirmation, matching the "key revoke"
// pattern. The restore reference is printed so the result can be matched later;
// with --wait the command watches marbor restart and exits nonzero unless the
// restore is reported as done.
func runBackupRestore(flags *globalFlags, filename string, yes, wait bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("restore backup (marbor will restart)", filename, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	before := ""
	if h, herr := client.Health(); herr == nil {
		before = h.StartedAt
	}
	ref, err := client.RestoreBackup(filename)
	if err != nil {
		return reportError(err, stderr)
	}
	result := map[string]interface{}{"ok": true, "file": filename, "restarting": true}
	if ref != "" {
		result["ref"] = ref
	}
	if !wait {
		if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
			return code
		}
		fmt.Fprintf(stdout, "restore from %q requested - marbor is restarting%s\n", filename, refSuffix(ref))
		fmt.Fprintln(stdout, "check the outcome with 'marbor backup list' once marbor is back, or use --wait next time")
		return ExitOK
	}
	if !flags.jsonOutput {
		fmt.Fprintf(stdout, "restore from %q requested%s - waiting for marbor to restart\n", filename, refSuffix(ref))
	}
	outcome, code := waitForRestore(client, ref, before)
	result["result"] = outcome.result
	result["confirmed"] = outcome.confirmed
	if outcome.detail != "" {
		result["detail"] = outcome.detail
	}
	if code != ExitOK {
		result["ok"] = false
	}
	if handled, _ := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		if code != ExitOK {
			fmt.Fprintln(stderr, "error: "+outcome.detail)
		}
		return code
	}
	if code != ExitOK {
		fmt.Fprintln(stderr, "error: "+outcome.detail)
		return code
	}
	fmt.Fprintln(stdout, outcome.detail)
	return ExitOK
}

func refSuffix(ref string) string {
	if ref == "" {
		return ""
	}
	return " (ref " + ref + ")"
}

// restoreOutcome is what `restore --wait` learned. confirmed is false when the
// result was inferred rather than read from marbor.
type restoreOutcome struct {
	result    string
	confirmed bool
	detail    string
}

// waitForRestore waits for marbor to come back as a new process (its start time
// from /health differs from before; without one, marbor must have been seen down
// first - a 503 answer is a running marbor, not a down one) and then reads the
// restore's result. A restore that failed leaves the old database in place, so
// the session still works and the result is read; a session that no longer
// exists means the restored database replaced it, which is reported as an
// unconfirmed success.
func waitForRestore(client *Client, ref, before string) (restoreOutcome, int) {
	deadline := time.Now().Add(restoreWaitTimeout)
	sawDown := false
	var lastProblem string
	for time.Now().Before(deadline) {
		time.Sleep(restorePollInterval)
		h, err := client.Health()
		if err != nil {
			sawDown = true
			continue
		}
		restarted := h.StartedAt != "" && before != "" && h.StartedAt != before
		if !restarted && !(sawDown && (before == "" || h.StartedAt == "")) {
			continue
		}
		_, status, lerr := client.ListBackups()
		var cliErr *CLIError
		switch {
		case lerr == nil:
		case errors.As(lerr, &cliErr) && cliErr.Code == ExitAuthError:
			return restoreOutcome{result: "success", confirmed: false,
				detail: "marbor restarted and your session is no longer valid, which means the restored database is in use; run 'marbor login' and then 'marbor backup list' to confirm the result" + refSuffix(ref)}, ExitOK
		default:
			lastProblem = lerr.Error()
			continue
		}
		r := status.LastRestore
		if r == nil || (ref != "" && r.Ref != ref) {
			return restoreOutcome{result: "unknown", detail: "marbor is back but did not report the result of this restore" + refSuffix(ref) + "; check 'marbor backup list'"}, ExitServerError
		}
		if r.Result == "success" {
			detail := "restore finished: marbor is running on the restored database" + refSuffix(r.Ref)
			if r.Note != "" {
				detail += "; " + r.Note
			}
			return restoreOutcome{result: "success", confirmed: true, detail: detail}, ExitOK
		}
		detail := "restore " + r.Result
		if r.Error != "" {
			detail += ": " + r.Error
		}
		if r.Note != "" {
			detail += "; " + r.Note
		}
		return restoreOutcome{result: r.Result, confirmed: true, detail: detail + refSuffix(r.Ref)}, ExitServerError
	}
	detail := fmt.Sprintf("marbor did not come back within %s; check 'marbor backup list' once it is up", restoreWaitTimeout)
	if lastProblem != "" {
		detail += " (last error: " + lastProblem + ")"
	}
	return restoreOutcome{result: "timeout", detail: detail}, ExitServerError
}

// runBackupAck implements `marbor backup ack-restore`: show, then forget, the
// remembered outcome of the last restore. The reference of the result shown is
// the one sent, so a result that changed in between is left alone. Clearing is
// shared: it clears the result for every operator and the dashboard too.
func runBackupAck(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	_, status, err := client.ListBackups()
	if err != nil {
		return reportError(err, stderr)
	}
	if status.LastRestore == nil {
		if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "cleared": false}); handled {
			return code
		}
		fmt.Fprintln(stdout, "there is no restore result to clear")
		return ExitOK
	}
	ref := status.LastRestore.Ref
	if !validRestoreRef(ref) {
		ref = ""
	}
	if err := client.AcknowledgeRestoreResult(ref); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "cleared": true, "result": status.LastRestore.Result}); handled {
		return code
	}
	fmt.Fprintf(stdout, "restore result cleared (it was: %s%s); this clears it for every operator and the dashboard\n", status.LastRestore.Result, refSuffix(ref))
	return ExitOK
}

// runBackupDelete implements `marbor backup delete <name> [--yes]`: remove one
// stored backup file from the server. Destructive, so it asks first.
func runBackupDelete(flags *globalFlags, name string, yes bool, stdout, stderr io.Writer) int {
	if !validBackupName(name) {
		return reportError(userErrorf("%q is not a marbor backup file name (expected marbor-backup-YYYYMMDD-HHMMSS.db.gz)", name), stderr)
	}
	if err := requireConfirm("delete backup (it cannot be recovered)", name, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DeleteBackup(name); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "file": name, "deleted": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "deleted %s\n", name)
	return ExitOK
}

// runBackupUpload implements `marbor backup upload --file <local-path>`.
func runBackupUpload(flags *globalFlags, path string, stdout, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot open %s: %v\n", path, err)
		return ExitUserError
	}
	defer f.Close()
	var size int64
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	warning, err := client.UploadBackup(path, f, size)
	if err != nil {
		return reportError(err, stderr)
	}
	result := map[string]interface{}{"ok": true, "file": path, "uploaded": true}
	if warning != "" {
		result["warning"] = warning
	}
	if warning != "" {
		fmt.Fprintln(stderr, "warning: "+warning)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
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
	fmt.Fprintf(stdout, "local requests: %d, cloud requests: %d, saved: %s, cloud spent: %s (all time)\n",
		s.LocalRequests, s.CloudRequests, savedStr, spentStr)
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
