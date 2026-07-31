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
