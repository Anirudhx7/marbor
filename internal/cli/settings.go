package cli

// settings.go - `marbor settings get/set` (GET/PUT /admin/settings had full
// UI coverage in Settings.tsx but no CLI). Deliberately whole-object,
// not per-field flags: config.Config is large (Admin/Routing/Docker/Audit/
// Webhook/Savings/Warmup/ContextWindows/CloudProviders/...) and the server
// already accepts a partial JSON payload merged onto the current config, so
// `settings get > file.json`, edit, `settings set --file file.json` mirrors
// how the Settings page itself round-trips the same object.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// runSettingsGet implements `marbor settings get`. Always prints the raw
// JSON response (secret fields pre-masked as "***" by the server) - same
// rationale as `marbor system-info`, not routed through emitJSON's table
// fallback since there is no sane tabular form for the full config.
func runSettingsGet(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	raw, err := client.Settings()
	if err != nil {
		return reportError(err, stderr)
	}
	fmt.Fprintln(stdout, string(raw))
	return ExitOK
}

// runSettingsSet implements `marbor settings set --file <path>`. Reads a
// JSON config.Config payload (typically a partial one - only the fields
// being changed) from a local file and PUTs it verbatim.
func runSettingsSet(flags *globalFlags, filePath string, stdout, stderr io.Writer) int {
	body, err := os.ReadFile(filePath)
	if err != nil {
		return reportError(fmt.Errorf("read %s: %w", filePath, err), stderr)
	}
	if !json.Valid(body) {
		return reportError(fmt.Errorf("%s does not contain valid JSON", filePath), stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.UpdateSettings(json.RawMessage(body)); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true}); handled {
		return code
	}
	fmt.Fprintln(stdout, "settings updated")
	return ExitOK
}
