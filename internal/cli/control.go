package cli

import (
	"fmt"
	"io"
)

// runNodeControlProbe implements `marbor node control probe <node>` - GET
// /admin/nodes/{name}/control, read-only.
func runNodeControlProbe(flags *globalFlags, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	info, err := client.NodeControlProbe(node)
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, info); handled {
		return code
	}

	if info.Configured {
		fmt.Fprintf(stdout, "%s: configured driver=%s identifier=%s\n", info.Node, info.Driver, info.Identifier)
	} else {
		fmt.Fprintf(stdout, "%s: not configured\n", info.Node)
	}
	if info.Discovered.Driver != "" {
		fmt.Fprintf(stdout, "discovered: driver=%s identifier=%s\n", info.Discovered.Driver, info.Discovered.Identifier)
		for _, e := range info.Discovered.Evidence {
			fmt.Fprintf(stdout, "  evidence: %s\n", e)
		}
	} else {
		fmt.Fprintln(stdout, "discovered: none")
	}
	return ExitOK
}

// runNodeControlAccept implements `marbor node control accept <node>
// --driver X --identifier Y [--start-command Z]` - POST /admin/nodes/{name}
// /control/accept, the operator's explicit confirmation of a control driver
// (never automatic - marbor-agent-capabilities.md section 5.6). Flag parsing
// happens in cli.go's dispatcher (same fs.Parse call that handles the
// global flags), so this function receives already-validated values.
func runNodeControlAccept(flags *globalFlags, node, driver, identifier, startCommand string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	if err := client.NodeControlAccept(node, driver, identifier, startCommand); err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": node, "driver": driver, "identifier": identifier,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "%s: control driver accepted (%s / %s)\n", node, driver, identifier)
	return ExitOK
}
