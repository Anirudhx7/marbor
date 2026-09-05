package cli

// benchmark.go - `marbor benchmark run/progress/cancel/runs` (all 4
// benchmark endpoints had full UI coverage in Benchmark.tsx but no CLI). Not
// to be confused with the
// separate standalone `cmd/bench` tool - this hits the Admin API's
// in-dashboard benchmark job, matching what the UI's "Run Benchmark" button
// does.

import (
	"fmt"
	"io"
)

func printBenchmarkUsage(w io.Writer) { writeHelp(w, findCommand(root(), "benchmark")) }

// runBenchmarkRun implements `marbor benchmark run <node> <model> [--n N]`.
func runBenchmarkRun(flags *globalFlags, node, model string, n int, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	jobID, err := client.RunBenchmark(node, model, n)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"job_id": jobID}); handled {
		return code
	}
	fmt.Fprintf(stdout, "benchmark started: job_id=%s\n", jobID)
	return ExitOK
}

// runBenchmarkProgress implements `marbor benchmark progress <job-id>` - a
// single snapshot read off the SSE progress stream (see
// Client.BenchmarkProgress's doc comment).
func runBenchmarkProgress(flags *globalFlags, jobID string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.BenchmarkProgress(jobID)
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runBenchmarkCancel implements `marbor benchmark cancel <job-id>`.
func runBenchmarkCancel(flags *globalFlags, jobID string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	cancelled, err := client.CancelBenchmark(jobID)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "cancelled": cancelled}); handled {
		return code
	}
	if cancelled {
		fmt.Fprintf(stdout, "benchmark %q cancelled\n", jobID)
	} else {
		fmt.Fprintf(stdout, "no running benchmark %q to cancel\n", jobID)
	}
	return ExitOK
}

// runBenchmarkRuns implements `marbor benchmark runs`. Prints raw JSON -
// see Client.ModelCatalog's doc comment for why (store.BenchmarkRun has
// many percentile/TPOT fields).
func runBenchmarkRuns(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.BenchmarkRuns()
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}
