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

	if flags.jsonOutput {
		if err := writeJSON(stdout, models); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitServerError
		}
		return ExitOK
	}

	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tWARM/TOTAL NODES\tVRAM\tDISK\tFAMILY\tDIGEST MISMATCH")
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
