package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildAgentBinary compiles cmd/marbor-agent into t.TempDir() once per
// test and returns the resulting path, so TestMain_Version/TestMain_ServiceUsage
// exercise the real compiled binary (argv dispatch, -version interception,
// delegation to internal/nodeagent.Run) rather than re-testing internal
// package functions already covered by internal/nodeagent's own tests.
func buildAgentBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "marbor-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/marbor-agent: %v\n%s", err, out)
	}
	return bin
}

func TestMain_VersionFlag(t *testing.T) {
	bin := buildAgentBinary(t)
	out, err := exec.Command(bin, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("-version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "marbor-agent") {
		t.Errorf("-version output = %q, want it to name marbor-agent", out)
	}
}

// TestMain_DelegatesUnknownSubcommandToNodeagent proves main() actually
// forwards argv into internal/nodeagent.Run (rather than, say, silently
// exiting 0) by triggering a subcommand-shaped error only nodeagent.Run
// itself can produce (agent.go's "unknown agent subcommand" check) - the
// same error a pre-split "ollama-mesh agent bogus" would have hit, now one
// argv position earlier.
func TestMain_DelegatesUnknownSubcommandToNodeagent(t *testing.T) {
	bin := buildAgentBinary(t)
	out, err := exec.Command(bin, "bogus-subcommand").CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for an unknown subcommand, got success, output:\n%s", out)
	}
	if !strings.Contains(string(out), "bogus-subcommand") {
		t.Errorf("expected the error to name the bad subcommand, got:\n%s", out)
	}
}
