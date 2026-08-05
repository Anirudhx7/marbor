package cli

import (
	"fmt"
	"io"
)

// runModels implements `mesh models` - GET /admin/v1/models, session-authed.
func runModels(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	models, err := client.Models()
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, models); handled {
		return code
	}

	// Header says "healthy", not "total", because ModelEntry.TotalNodes is
	// populated server-side (admin.go handleModels) from the count of
	// currently-healthy nodes, not the fleet's total node count - labeling
	// it "total" here would silently understate the fleet whenever any node
	// is unhealthy.
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tWARM/HEALTHY NODES\tVRAM\tDISK\tFAMILY\tDIGEST MISMATCH")
	for _, m := range models.Models {
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\t%s\n",
			m.Name, m.WarmCount, m.TotalNodes, fmtMB(m.SizeVRAM/1024/1024), fmtMB(m.SizeDisk/1024/1024),
			m.Family, yesNo(m.DigestMismatch))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runModelsPull implements `mesh models pull <node> <model>` - POST
// /admin/nodes/{name}/pull, capability "models.pull". The pull runs async
// server-side; this only confirms the job started (same contract as the UI's
// Models.tsx pull flow) - it does not block for completion.
func runModelsPull(flags *globalFlags, node, model string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	result, err := client.PullModel(node, model)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: pull started for %s\n", node, model)
	return ExitOK
}

// runModelsDelete implements `mesh models delete <node> <model>` - DELETE
// /admin/nodes/{name}/models/{model}, capability "models.delete".
func runModelsDelete(flags *globalFlags, node, model string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	if err := client.DeleteNodeModel(node, model); err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": node, "model": model,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: deleted %s\n", node, model)
	return ExitOK
}

// runModelsUnload implements `mesh models unload <node> <model>` - POST
// /admin/nodes/{name}/unload, capability "models.unload".
func runModelsUnload(flags *globalFlags, node, model string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	if err := client.UnloadModel(node, model); err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": node, "model": model,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: unloaded %s\n", node, model)
	return ExitOK
}

// runModelsList implements `mesh models list <node>` - GET
// /admin/nodes/{name}/models, capability "models.list" - the per-node local
// inventory, distinct from the bare `mesh models` fleet-wide summary above.
func runModelsList(flags *globalFlags, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	models, err := client.NodeModels(node)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, models); handled {
		return code
	}

	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tSIZE\tSOURCE\tFAMILY")
	for _, m := range models {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Name, fmtMB(m.SizeBytes/1024/1024), m.Source, m.Family)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
