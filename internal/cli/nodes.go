package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// parseVRAMOverrides parses a CLI --vram-override value of the form
// "model=mb[,model2=mb2...]" into a map[string]int64, validating positive
// integers client-side (fast-fail UX pre-check, matching
// isValidTLSFingerprintArg's role - the server remains the authority).
// An empty input string parses to a non-nil empty map (explicit clear).
func parseVRAMOverrides(s string) (map[string]int64, error) {
	out := map[string]int64{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --vram-override entry %q (want model=mb)", pair)
		}
		model := strings.TrimSpace(parts[0])
		if model == "" {
			return nil, fmt.Errorf("invalid --vram-override entry %q (empty model name)", pair)
		}
		mb, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || mb <= 0 {
			return nil, fmt.Errorf("invalid --vram-override entry %q (mb must be a positive integer)", pair)
		}
		out[model] = mb
	}
	return out, nil
}

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

// runNodesConfirmTLS implements `marbor nodes confirm-tls <node-name>
// --fingerprint=SHA256:...` (P24, spec section 11's headless-enrollment
// exception - the only CLI surface this item adds). fingerprint must come
// from the operator's own flag value; this command never probes the node or
// otherwise infers/accepts a certificate on its own - the caller is
// expected to have already read the value from "agent service status" on
// the node itself, or from the marbor's tls-probe endpoint via another
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

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": name, "tls_fingerprint": fingerprint,
	}); handled {
		return code
	}

	fmt.Fprintf(stdout, "node %q TLS fingerprint pinned: %s\n", name, fingerprint)
	return ExitOK
}

// runNodes implements `marbor nodes` - GET /admin/v1/nodes, session-authed.
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

// runNodesAdd implements `marbor nodes add <name> <url> [--runtime x]
// [--gpu-model x] [--vram-total-mb n]` - POST /admin/nodes (P-A2-01).
func runNodesAdd(flags *globalFlags, name, url, gpuModel, runtime string, vramTotalMB int64, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	created, err := client.AddNode(NodeAddRequest{
		Name:        name,
		URL:         url,
		GPUModel:    gpuModel,
		VRAMTotalMB: vramTotalMB,
		Runtime:     runtime,
	})
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": name, "created": created,
	}); handled {
		return code
	}
	if created {
		fmt.Fprintf(stdout, "node %q added\n", name)
	} else {
		fmt.Fprintf(stdout, "node %q updated\n", name)
	}
	return ExitOK
}

// runNodesRemove implements `marbor nodes remove <name> [--yes]` - DELETE
// /admin/nodes/{name} (P-A2-01). Destructive per R10: requires --yes or an
// interactive TTY confirmation, matching the "key revoke"/"users delete"
// pattern (confirm.go).
func runNodesRemove(flags *globalFlags, name string, yes bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("remove node", name, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DeleteNode(name); err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "node": name, "removed": true,
	}); handled {
		return code
	}
	fmt.Fprintf(stdout, "node %q removed\n", name)
	return ExitOK
}

// parseCommaList splits a comma-separated flag value into a trimmed,
// non-empty-entry slice. An empty input string returns a non-nil empty
// slice (explicit clear), matching parseVRAMOverrides' convention.
func parseCommaList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// runNodesWarmupGet implements `marbor nodes warmup get <node>` - GET
// /admin/nodes/{name}/warmup (P-A2-02).
func runNodesWarmupGet(flags *globalFlags, name string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	info, err := client.GetNodeWarmup(name)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, info); handled {
		return code
	}
	fmt.Fprintf(stdout, "node %q warmup enabled=%v models=%v\n", name, info.Enabled, info.Models)
	return ExitOK
}

// runNodesWarmupSet implements `marbor nodes warmup set <node> --enabled
// true|false --models a,b` - PUT /admin/nodes/{name}/warmup (P-A2-02).
func runNodesWarmupSet(flags *globalFlags, name string, enabled bool, models string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	info, err := client.SetNodeWarmup(name, enabled, parseCommaList(models))
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, info); handled {
		return code
	}
	fmt.Fprintf(stdout, "node %q warmup set: enabled=%v models=%v\n", name, info.Enabled, info.Models)
	return ExitOK
}

// runNodesPinnedGet implements `marbor nodes pinned get <node>` - GET
// /admin/nodes/{name}/pinned (P-A2-02).
func runNodesPinnedGet(flags *globalFlags, name string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	models, err := client.GetPinned(name)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"models": models}); handled {
		return code
	}
	fmt.Fprintf(stdout, "node %q pinned models: %v\n", name, models)
	return ExitOK
}

// runNodesPinnedSet implements `marbor nodes pinned set <node> --models
// a,b` - PUT /admin/nodes/{name}/pinned (whole-list replace, P-A2-02).
func runNodesPinnedSet(flags *globalFlags, name, models string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	out, err := client.SetPinned(name, parseCommaList(models))
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"models": out}); handled {
		return code
	}
	fmt.Fprintf(stdout, "node %q pinned models set: %v\n", name, out)
	return ExitOK
}

// runNodesPrewarmSet implements `marbor nodes prewarm set <node> --disabled
// true|false` - POST /admin/nodes/{name}/prewarm (P-A2-02).
func runNodesPrewarmSet(flags *globalFlags, name string, disabled bool, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.SetNodePrewarm(name, disabled); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"node": name, "prewarm_disabled": disabled,
	}); handled {
		return code
	}
	fmt.Fprintf(stdout, "node %q prewarm disabled=%v\n", name, disabled)
	return ExitOK
}

// runNodesPatchWithCtx implements `marbor nodes patch <node> --parallelism-type tp --parallelism-width 8` (P397)
// and `marbor nodes patch <node> --vram-override model=mb[,model2=mb2]` (P411).
func runNodesPatchWithCtx(ctx *RunCtx, name string) int {
	pTypeSet := ctx.IsSet("parallelism-type")
	pWidthSet := ctx.IsSet("parallelism-width")
	pType := ctx.String("parallelism-type")
	pWidth := ctx.Int("parallelism-width")
	vramOverrideSet := ctx.IsSet("vram-override")
	var vramOverrides map[string]int64
	if vramOverrideSet {
		var err error
		vramOverrides, err = parseVRAMOverrides(ctx.String("vram-override"))
		if err != nil {
			fmt.Fprintf(ctx.Stderr, "error: %v\n", err)
			return ExitUserError
		}
	}
	if !pTypeSet && !pWidthSet && !vramOverrideSet {
		fmt.Fprintln(ctx.Stderr, "error: at least one of --parallelism-type, --parallelism-width, or --vram-override is required")
		return ExitUserError
	}
	// For clearing, both must be explicitly set to empty/0
	if pTypeSet != pWidthSet {
		fmt.Fprintln(ctx.Stderr, "error: --parallelism-type and --parallelism-width must be set together or cleared together")
		return ExitUserError
	}
	if pType != "" {
		switch pType {
		case "tp", "pp", "ep", "dp":
		default:
			fmt.Fprintf(ctx.Stderr, "error: --parallelism-type must be one of tp, pp, ep, dp (got %q)\n", pType)
			return ExitUserError
		}
	}
	if pWidth < 0 || pWidth > 64 {
		fmt.Fprintf(ctx.Stderr, "error: --parallelism-width must be between 0 and 64 (got %d)\n", pWidth)
		return ExitUserError
	}
	client, err := authenticatedClient(ctx.Flags)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	// Client needs to know if fields were visited to send nil vs omit.
	// We use pointers: nil = not visited, pointer to value = visited (including empty/0 for clear).
	var pTypePtr *string
	var pWidthPtr *int
	if pTypeSet {
		v := pType
		pTypePtr = &v
	}
	if pWidthSet {
		v := pWidth
		pWidthPtr = &v
	}
	var vramOverridesPtr *map[string]int64
	if vramOverrideSet {
		vramOverridesPtr = &vramOverrides
	}
	if err := client.PatchNodeFieldsWithPtr(name, pTypePtr, pWidthPtr, vramOverridesPtr); err != nil {
		return reportError(err, ctx.Stderr)
	}
	result := map[string]interface{}{"ok": true, "node": name}
	if pTypeSet || pWidthSet {
		result["parallelism_type"] = pType
		result["parallelism_width"] = pWidth
	}
	if vramOverrideSet {
		result["vram_overrides"] = vramOverrides
	}
	if handled, code := emitJSON(ctx.Stdout, ctx.Stderr, ctx.Flags.jsonOutput, result); handled {
		return code
	}
	if pTypeSet || pWidthSet {
		if pType == "" {
			fmt.Fprintf(ctx.Stdout, "node %q parallelism cleared\n", name)
		} else {
			fmt.Fprintf(ctx.Stdout, "node %q parallelism set to %s=%d\n", name, pType, pWidth)
		}
	}
	if vramOverrideSet {
		if len(vramOverrides) == 0 {
			fmt.Fprintf(ctx.Stdout, "node %q vram overrides cleared\n", name)
		} else {
			fmt.Fprintf(ctx.Stdout, "node %q vram overrides set: %v\n", name, vramOverrides)
		}
	}
	return ExitOK
}
