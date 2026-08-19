package cli

import (
	"bytes"
	"strings"
	"testing"
)

// completion_test.go tests the "ollama-mesh completion bash|zsh|fish"
// command (completion.go, registry_tree.go's completionCmd()) - P83+ CLI
// hardening plan, Implementation section 7.

func runCLI(args ...string) (stdout, stderr string, code int) {
	var outBuf, errBuf bytes.Buffer
	code = Run(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

func TestRun_Completion_Bash(t *testing.T) {
	stdout, stderr, code := runCLI("completion", "bash")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr = %q", code, ExitOK, stderr)
	}
	if stdout == "" {
		t.Fatal("stdout is empty")
	}
	for _, want := range []string{"models", "runtime", "complete -F _ollama_mesh ollama-mesh"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("bash completion missing %q\n--- stdout ---\n%s", want, stdout)
		}
	}
}

func TestRun_Completion_Zsh(t *testing.T) {
	stdout, stderr, code := runCLI("completion", "zsh")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr = %q", code, ExitOK, stderr)
	}
	if stdout == "" {
		t.Fatal("stdout is empty")
	}
	for _, want := range []string{"#compdef ollama-mesh", "models", "runtime"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("zsh completion missing %q\n--- stdout ---\n%s", want, stdout)
		}
	}
}

func TestRun_Completion_Fish(t *testing.T) {
	stdout, stderr, code := runCLI("completion", "fish")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr = %q", code, ExitOK, stderr)
	}
	if stdout == "" {
		t.Fatal("stdout is empty")
	}
	for _, want := range []string{"complete -c ollama-mesh", "models", "runtime"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("fish completion missing %q\n--- stdout ---\n%s", want, stdout)
		}
	}
}

func TestRun_Completion_UnknownShell(t *testing.T) {
	stdout, stderr, code := runCLI("completion", "csh")
	if code != ExitUserError {
		t.Fatalf("exit code = %d, want %d", code, ExitUserError)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty on error, got %q", stdout)
	}
	if !strings.Contains(stderr, `unknown shell "csh"`) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, `unknown shell "csh"`)
	}
}

func TestRun_Completion_Deterministic(t *testing.T) {
	r := root()

	if a, b := generateBashCompletion(r), generateBashCompletion(r); a != b {
		t.Error("generateBashCompletion is not deterministic across two calls")
	}
	if a, b := generateZshCompletion(r), generateZshCompletion(r); a != b {
		t.Error("generateZshCompletion is not deterministic across two calls")
	}
	if a, b := generateFishCompletion(r), generateFishCompletion(r); a != b {
		t.Error("generateFishCompletion is not deterministic across two calls")
	}
}

func TestCompletion_HiddenFromRootHelp(t *testing.T) {
	stdout, _, code := runCLI("--help")
	if code != ExitOK {
		t.Fatalf("--help exit code = %d, want %d", code, ExitOK)
	}
	if strings.Contains(stdout, "completion") {
		t.Errorf("--help output contains \"completion\" - Hidden is not being respected\n--- stdout ---\n%s", stdout)
	}

	// Hidden must never mean unreachable.
	compOut, stderr, compCode := runCLI("completion", "bash")
	if compCode != ExitOK {
		t.Fatalf("completion bash exit code = %d, want %d; stderr = %q", compCode, ExitOK, stderr)
	}
	if compOut == "" {
		t.Fatal("completion bash produced no output despite being Hidden")
	}
}
