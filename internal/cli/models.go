package cli

import (
	"fmt"
	"io"
)

// runModels implements `marbor models` - GET /admin/v1/models, session-authed.
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
	fmt.Fprintln(tw, "NAME\tWARM/HEALTHY NODES\tVRAM\tTOTAL VRAM\tDISK\tFAMILY\tDRIFT\tDIGEST MISMATCH")
	for _, m := range models.Models {
		totalVRAM := "-"
		if m.TotalVRAMBytes > 0 {
			totalVRAM = fmtMB(m.TotalVRAMBytes / 1024 / 1024)
		} else if m.WarmCount > 0 && m.SizeVRAM > 0 {
			// Fallback for mixed-version server that hasn't yet populated
			// total_vram_bytes - derive from per-copy size (R1 fallback, not
			// estimate: shown only when warm copies exist and a real per-copy
			// figure is known).
			totalVRAM = fmtMB(int64(m.WarmCount) * m.SizeVRAM / 1024 / 1024)
		}
		drift := m.DriftDetails
		if drift == "" {
			drift = "-"
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Name, m.WarmCount, m.TotalNodes, fmtMB(m.SizeVRAM/1024/1024), totalVRAM, fmtMB(m.SizeDisk/1024/1024),
			m.Family, drift, yesNo(m.DigestMismatch))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runModelsFleet implements `marbor models fleet [--drifted-only]` - same GET
// /admin/v1/models fleet aggregation as bare `marbor models`, but with the
// fleet-first columns and an optional drifted-only filter mirroring the UI's
// drift toggle. No new Admin API route - one endpoint, same live data (Law 6).
func runModelsFleet(flags *globalFlags, driftedOnly bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	models, err := client.Models()
	if err != nil {
		return reportError(err, stderr)
	}

	filtered := models.Models
	if driftedOnly {
		out := make([]ModelEntry, 0, len(models.Models))
		for _, m := range models.Models {
			if m.DigestMismatch {
				out = append(out, m)
			}
		}
		filtered = out
		// Preserve the wrapper's metadata but swap the list for filtered.
		models.Models = filtered
		models.TotalModels = len(filtered)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, models); handled {
		return code
	}

	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tWARM/HEALTHY\tTOTAL VRAM\tDRIFT\tNODES")
	for _, m := range filtered {
		totalVRAM := "-"
		if m.TotalVRAMBytes > 0 {
			totalVRAM = fmtMB(m.TotalVRAMBytes / 1024 / 1024)
		} else if m.WarmCount > 0 && m.SizeVRAM > 0 {
			totalVRAM = fmtMB(int64(m.WarmCount) * m.SizeVRAM / 1024 / 1024)
		}
		drift := m.DriftDetails
		if drift == "" {
			drift = "-"
		}
		// Nodes column: compact "gpu-01(warm,ollama) gpu-02(cold)" - mirrors
		// the UI's node chips but in a single shell-friendly column.
		nodeParts := make([]string, 0, len(m.Nodes))
		for _, n := range m.Nodes {
			part := n.Name
			if n.Warm {
				part += "(warm"
			} else {
				part += "(cold"
			}
			if n.Runtime != "" {
				part += "," + n.Runtime
			}
			part += ")"
			nodeParts = append(nodeParts, part)
		}
		nodesStr := "-"
		if len(nodeParts) > 0 {
			// Join with comma for single-line shell output; tabwriter already
			// separates columns by tabs, so commas keep node list as one column.
			nodesStr = ""
			for i, p := range nodeParts {
				if i > 0 {
					nodesStr += ","
				}
				nodesStr += p
			}
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\n",
			m.Name, m.WarmCount, m.TotalNodes, totalVRAM, drift, nodesStr)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}

// runModelsPull implements `marbor models pull <node> <model>` - POST
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

// runModelsDelete implements `marbor models delete <node> <model>` - DELETE
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

// runModelsUnload implements `marbor models unload <node> <model>` - POST
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

// runModelsList implements `marbor models list <node>` - GET
// /admin/nodes/{name}/models, capability "models.list" - the per-node local
// inventory, distinct from the bare `marbor models` fleet-wide summary above.
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
