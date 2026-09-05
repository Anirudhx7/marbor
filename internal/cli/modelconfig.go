package cli

// modelconfig.go - `marbor model-config get/set/delete/list/capabilities`
// and `marbor nodes fit` (all 5 model-config endpoints plus GET
// /admin/nodes/model-fit had full UI coverage in GPUNodes.tsx's
// ModelConfigModal / fit drawer but no CLI).
//
// store.ModelConfig has ~40 optional per-runtime sampling/load-time fields
// (temperature, top_p, num_ctx, mirostat, dry_*, xtc_*, rpm/tpm, ...). This
// CLI deliberately does not expose 40 individual flags for "set" - that
// would be a large, narrow, easy-to-drift surface for a rarely-hand-typed
// config blob. Instead "set" takes the same JSON shape the Admin API
// accepts via --from-json (a literal JSON object or @path/to/file.json),
// which is both smaller to build and forward-compatible with new
// ModelConfig fields without a CLI code change (a documented, verify-before-
// build-stated scope choice, not a silent omission).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func printModelConfigUsage(w io.Writer) { writeHelp(w, findCommand(root(), "model-config")) }

// readJSONArg resolves a --from-json flag value: "@path" reads the file at
// path, anything else is treated as a literal JSON string.
func readJSONArg(raw string) (json.RawMessage, error) {
	if strings.HasPrefix(raw, "@") {
		data, err := os.ReadFile(raw[1:])
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", raw[1:], err)
		}
		return json.RawMessage(data), nil
	}
	return json.RawMessage(raw), nil
}

// runModelConfigGet implements `marbor model-config get --model --node`.
func runModelConfigGet(flags *globalFlags, model, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.GetModelConfig(model, node)
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runModelConfigSet implements `marbor model-config set --from-json
// '{"model":"...","node":"...","temperature":0.7}'` or `--from-json
// @profile.json`.
func runModelConfigSet(flags *globalFlags, fromJSON string, stdout, stderr io.Writer) int {
	body, err := readJSONArg(fromJSON)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUserError
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	out, err := client.SetModelConfig(body)
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(out))
	return ExitOK
}

// runModelConfigDelete implements `marbor model-config delete --model --node`.
func runModelConfigDelete(flags *globalFlags, model, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DeleteModelConfig(model, node); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "model": model, "node": node, "deleted": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "model config for %q on %q deleted\n", model, node)
	return ExitOK
}

// runModelConfigList implements `marbor model-config list`.
func runModelConfigList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.ListModelConfigs()
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runModelConfigCapabilities implements `marbor model-config capabilities`.
func runModelConfigCapabilities(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	caps, err := client.ModelConfigCapabilities()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, caps); handled {
		return code
	}
	for _, runtime := range []string{"ollama", "vllm", "tgi", "llamacpp", "mlx"} {
		fmt.Fprintf(stdout, "%s: %v\n", runtime, caps[runtime])
	}
	return ExitOK
}

// runNodesFit implements `marbor nodes fit` - GET /admin/nodes/model-fit.
func runNodesFit(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	nodes, err := client.ModelFit()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, nodes); handled {
		return code
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "NODE\tVRAM FREE/TOTAL\tSOURCE\tMODEL\tFIT\tLOADED")
	for _, n := range nodes {
		if len(n.Models) == 0 {
			fmt.Fprintf(tw, "%s\t%s / %s\t%s\t-\t-\t-\n", n.Name, fmtMB(n.VRAMFreeBytes/(1024*1024)), fmtMB(n.VRAMTotalBytes/(1024*1024)), n.VRAMSource)
			continue
		}
		for _, m := range n.Models {
			fmt.Fprintf(tw, "%s\t%s / %s\t%s\t%s\t%s\t%v\n", n.Name, fmtMB(n.VRAMFreeBytes/(1024*1024)), fmtMB(n.VRAMTotalBytes/(1024*1024)), n.VRAMSource, m.Name, m.Fit, m.Loaded)
		}
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	return ExitOK
}
