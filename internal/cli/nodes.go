package cli

import (
	"fmt"
	"io"
)

// runNodes implements `mesh nodes` - GET /admin/v1/nodes, session-authed.
func runNodes(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	nodes, err := client.Nodes()
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, nodes); handled {
		return code
	}

	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NAME\tHOST:PORT\tHEALTH\tRUNTIME\tGPU\tVRAM USED/TOTAL\tMODELS WARM\tDRAINING")
	for _, n := range nodes {
		fmt.Fprintf(tw, "%s\t%s:%d\t%s\t%s\t%s\t%s / %s\t%d\t%s\n",
			n.Name, n.Host, n.Port, n.Health, n.Runtime, n.GPUModel,
			fmtMB(n.VRAMUsedMB), fmtMB(n.VRAMTotalMB), len(n.LoadedModels), yesNo(n.Draining))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
