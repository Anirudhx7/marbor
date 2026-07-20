package nodeagent

// runtime_version.go detects the locally-running inference runtime's own
// reported version, for RuntimeInfo.Version. Only "ollama" has a real
// single-command version query today; runtimes without an equivalent
// primitive are left empty (never guessed - R1) until one is found.

import (
	"context"
	"os/exec"
	"strings"
)

// runtimeVersionCommands maps a detected runtime name to a function that
// returns its version string, or "" if it couldn't be determined. Add a
// runtime here only once a real version-query primitive for it exists -
// never a guessed/derived value.
var runtimeVersionCommands = map[string]func(ctx context.Context) string{
	"ollama": detectOllamaVersion,
}

// detectRuntimeVersion looks up and runs the version command for name, if
// one is known. Returns "" (omitted from the wire payload via omitempty)
// when none exists or the command fails - never fabricated.
func detectRuntimeVersion(ctx context.Context, name string) string {
	fn, ok := runtimeVersionCommands[name]
	if !ok {
		return ""
	}
	return fn(ctx)
}

// detectOllamaVersion runs `ollama version`, whose output is a line like
// "ollama version 0.6.5" (or "client version 0.6.5" pre-server-connect) -
// the last whitespace-separated token is the semver part in every observed
// format.
func detectOllamaVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "ollama", "version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}
