package cli

import (
	"fmt"
	"io"
	"strings"
)

// isValidTLSFingerprintArg mirrors internal/admin/admin.go's
// isValidTLSFingerprint (server-side validation) so an obviously malformed
// --fingerprint value fails fast locally with a clear message instead of a
// round-trip to the server for the same rejection. The server remains the
// authority - this is a UX fast-path, not a replacement for its check.
func isValidTLSFingerprintArg(s string) bool {
	const prefix = "SHA256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hex := s[len(prefix):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// runNodesConfirmTLS implements `mesh nodes confirm-tls <node-name>
// --fingerprint=SHA256:...` (P24, spec section 11's headless-enrollment
// exception - the only CLI surface this item adds). fingerprint must come
// from the operator's own flag value; this command never probes the node or
// otherwise infers/accepts a certificate on its own - the caller is
// expected to have already read the value from "agent service status" on
// the node itself, or from the mesh's tls-probe endpoint via another
// client, and independently confirmed it out of band.
func runNodesConfirmTLS(flags *globalFlags, name, fingerprint string, stdout, stderr io.Writer) int {
	if !isValidTLSFingerprintArg(fingerprint) {
		fmt.Fprintf(stderr, "invalid --fingerprint %q (want SHA256:<64 hex characters>)\n", fingerprint)
		return ExitUserError
	}

	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.PatchNodeTLSFingerprint(name, fingerprint); err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintf(stdout, "node %q TLS fingerprint pinned: %s\n", name, fingerprint)
	return ExitOK
}

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
