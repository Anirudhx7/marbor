package cli

// requests.go - per-request routing explainability CLI surface. First
// command touching the requests/audit surface (see admin's GET /admin/v1/
// requests and /admin/v1/audit, previously unreached from the CLI).

import (
	"fmt"
	"io"
)

// excludedReasonText translates ExcludedCandidate.Reason (a stable,
// machine-readable identifier from the Admin API) into the sentence shown to
// an operator. Kept as a local lookup rather than trusting the wire value to
// already be display text, matching the same six identifiers
// internal/router/explain.go defines.
func excludedReasonText(reason string) string {
	switch reason {
	case "unhealthy":
		return "node is unhealthy"
	case "draining":
		return "node is draining"
	case "runtime_mismatch":
		return "runtime does not match the request"
	case "ineligible_model":
		return "model not loaded on this node"
	case "over_capacity":
		return "over the per-node request cap"
	case "insufficient_gpu_group":
		return "insufficient GPUs for this model's parallelism requirement"
	default:
		return reason
	}
}

// printRequestsUsage is a thin wrapper over the registry-backed writeHelp
// (help.go).
func printRequestsUsage(w io.Writer) { writeHelp(w, findCommand(root(), "requests")) }

// runRequestsList implements `marbor requests list` - GET /admin/requests,
// the full in-memory request log ring, newest first.
func runRequestsList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	reqs, err := client.Requests()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, reqs); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "TIME\tID\tKEY\tMODEL\tNODE\tSTATUS\tLATENCY MS\tPREFILL MS\tCLOUD\tREASON")
	for _, r := range reqs {
		prefill := "-"
		if r.PrefillMs > 0 {
			prefill = fmt.Sprintf("%d", r.PrefillMs)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			r.Time.Format("2006-01-02T15:04:05Z07:00"), r.ID, r.KeyName, r.Model, r.Node, r.Status, r.LatencyMs, prefill, yesNo(r.Cloud), r.RoutingReason)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runRequestsLive implements `marbor requests live` - GET
// /admin/requests/live, the same bounded ring in its raw shape.
func runRequestsLive(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	reqs, err := client.LiveRequests()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, reqs); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "TIME\tID\tMODEL\tNODE\tSTATUS\tHTTP\tLATENCY MS\tTOK/S")
	for _, r := range reqs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%.1f\n",
			r.Time.Format("2006-01-02T15:04:05Z07:00"), r.ID, r.Model, r.Node, r.Status, r.HTTPStatus, r.Latency, r.TokensPerSec)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

func runRequestsExplain(flags *globalFlags, requestID string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	decision, err := client.ExplainRequest(requestID)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, decision); handled {
		return code
	}

	fmt.Fprintf(stdout, "Node:   %s\n", decision.Node)
	fmt.Fprintf(stdout, "Reason: %s\n", decision.Reason)
	if decision.Detail != "" {
		fmt.Fprintf(stdout, "Detail: %s\n", decision.Detail)
	}
	if decision.AffinityLost {
		fmt.Fprintln(stdout, "Note:   session affinity existed for this request but did not validate")
	}

	if len(decision.Components) > 0 {
		fmt.Fprintf(stdout, "Score:  %.2f\n\n", decision.Score)
		tw := newTabWriter(stdout)
		fmt.Fprintln(tw, "COMPONENT\tRAW\tWEIGHT\tVALUE\tPHASE")
		for _, c := range decision.Components {
			fmt.Fprintf(tw, "%s\t%.3f\t%.1f\t%.2f\t%s\n", c.Name, c.Raw, c.Weight, c.Value, c.Phase)
		}
		if err := tw.Flush(); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitServerError
		}
	}

	if len(decision.Excluded) > 0 {
		fmt.Fprintln(stdout, "\nEXCLUDED")
		tw := newTabWriter(stdout)
		fmt.Fprintln(tw, "NODE\tREASON")
		for _, e := range decision.Excluded {
			fmt.Fprintf(tw, "%s\t%s\n", e.Node, excludedReasonText(e.Reason))
		}
		if err := tw.Flush(); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitServerError
		}
		if decision.ExcludedTotal > len(decision.Excluded) {
			fmt.Fprintf(stdout, "...and %d more\n", decision.ExcludedTotal-len(decision.Excluded))
		}
	}

	return ExitOK
}
