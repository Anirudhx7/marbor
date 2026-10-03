package marboragent

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// startup runs the real foreground flag parsing and validation with a valid
// token, returning the parsed flags, the error, and the captured output.
func startup(t *testing.T, args ...string) (agentFlags, error, string, string) {
	t.Helper()
	t.Setenv("MARBOR_AGENT_SECRET", "test-token")
	var stdout, stderr bytes.Buffer
	af, err := parseAgentFlags(args, &stdout, &stderr)
	return af, err, stdout.String(), stderr.String()
}

func TestStartupRefusesPlaintextNonLoopback(t *testing.T) {
	for _, args := range [][]string{
		{"--port=9200"},
		{"--bind="},
		{"--bind=0.0.0.0"},
		{"--bind=192.168.1.20"},
		{"--bind=::"},
	} {
		_, err, _, _ := startup(t, args...)
		if err == nil || !strings.Contains(err.Error(), "refusing to serve plaintext HTTP") {
			t.Errorf("args %v: want plaintext refusal, got %v", args, err)
		}
	}
}

func TestStartupRefusesHostnameBind(t *testing.T) {
	for _, bind := range []string{"localhost", "example.com"} {
		_, err, _, _ := startup(t, "--bind="+bind)
		if err == nil || !strings.Contains(err.Error(), "refusing to serve plaintext HTTP") {
			t.Errorf("bind %q: want plaintext refusal, got %v", bind, err)
		}
	}
}

func TestStartupAllowsPlaintextWithOptIn(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "192.168.1.20"} {
		af, err, _, _ := startup(t, "--bind="+bind, "--allow-insecure-plaintext")
		if err != nil {
			t.Errorf("bind %q with opt-in: unexpected error %v", bind, err)
		}
		if !af.allowPlaintext || af.bind != bind {
			t.Errorf("bind %q: parsed flags wrong: %+v", bind, af)
		}
	}
}

func TestStartupAllowsLoopbackIPv4AndIPv6(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "::1", "[::1]"} {
		af, err, _, _ := startup(t, "--bind="+bind)
		if err != nil {
			t.Errorf("bind %q: unexpected error %v", bind, err)
		}
		if af.bind != bind || af.port != 9200 || af.token != "test-token" {
			t.Errorf("bind %q: parsed flags wrong: %+v", bind, af)
		}
	}
}

// Certificate and key paths are only checked for presence at validation time;
// the files are opened later by the TLS listener, so they need not exist here.
func TestStartupAllowsCertAndKey(t *testing.T) {
	af, err, _, _ := startup(t, "--cert=/nonexistent/c.pem", "--key=/nonexistent/k.pem")
	if err != nil {
		t.Fatalf("cert+key on all interfaces: unexpected error %v", err)
	}
	if af.cert != "/nonexistent/c.pem" || af.key != "/nonexistent/k.pem" {
		t.Errorf("parsed flags wrong: %+v", af)
	}
}

func TestStartupRefusesLoneCertOrKey(t *testing.T) {
	for _, args := range [][]string{
		{"--cert=/x/c.pem", "--bind=127.0.0.1"},
		{"--key=/x/k.pem", "--bind=127.0.0.1"},
	} {
		_, err, _, _ := startup(t, args...)
		if err == nil || !strings.Contains(err.Error(), "exactly one of --cert/--key") {
			t.Errorf("args %v: want lone cert/key refusal, got %v", args, err)
		}
	}
}

func TestStartupRefusesBindWithPort(t *testing.T) {
	for _, bind := range []string{"127.0.0.1:9200", "[::1]:9200", "0.0.0.0:80"} {
		_, err, _, _ := startup(t, "--bind="+bind, "--allow-insecure-plaintext")
		if err == nil || !strings.Contains(err.Error(), "--bind takes a host or IP without a port") {
			t.Errorf("bind %q: want bind-with-port refusal, got %v", bind, err)
		}
	}
}

func TestStartupErrorIsActionable(t *testing.T) {
	_, err, _, _ := startup(t, "--port=9200")
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"--cert", "--key", "marbor-agent service install", "--bind=127.0.0.1", "--allow-insecure-plaintext"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestStartupRequiresTokenAndPositiveInterval(t *testing.T) {
	t.Setenv("MARBOR_AGENT_SECRET", "")
	if _, err := parseAgentFlags([]string{"--bind=127.0.0.1"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "MARBOR_AGENT_SECRET") {
		t.Errorf("missing token: got %v", err)
	}
	if _, err, _, _ := startup(t, "--bind=127.0.0.1", "--refresh-interval=0s"); err == nil || !strings.Contains(err.Error(), "--refresh-interval must be positive") {
		t.Errorf("zero interval: got %v", err)
	}
}

func TestStartupReadRuntimeEnvDefault(t *testing.T) {
	t.Setenv("MARBOR_AGENT_READ_RUNTIME_ENV", "1")
	af, err, _, _ := startup(t, "--bind=127.0.0.1")
	if err != nil || !af.readRuntimeEnv {
		t.Errorf("env=1: readRuntimeEnv=%v err=%v, want true", af.readRuntimeEnv, err)
	}
	af, err, _, _ = startup(t, "--bind=127.0.0.1", "--read-runtime-env=false")
	if err != nil || af.readRuntimeEnv {
		t.Errorf("explicit false: readRuntimeEnv=%v err=%v, want false", af.readRuntimeEnv, err)
	}
	t.Setenv("MARBOR_AGENT_READ_RUNTIME_ENV", "")
	af, err, _, _ = startup(t, "--bind=127.0.0.1")
	if err != nil || af.readRuntimeEnv {
		t.Errorf("env unset: readRuntimeEnv=%v err=%v, want false", af.readRuntimeEnv, err)
	}
}

func TestStartupHelpReturnsUsage(t *testing.T) {
	for _, h := range []string{"-h", "--help"} {
		_, err, stdout, stderr := startup(t, h)
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s: want flag.ErrHelp, got %v", h, err)
		}
		if stderr != "" {
			t.Errorf("%s: help must not write to stderr, got %q", h, stderr)
		}
		for _, want := range []string{"Usage:", "-port", "-cert", "-key", "-bind", "-allow-insecure-plaintext", "-refresh-interval", "-read-runtime-env"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("%s: usage missing %q", h, want)
			}
		}
	}
}

func TestStartupUnknownFlagIsAnError(t *testing.T) {
	_, err, stdout, stderr := startup(t, "--no-such-flag")
	var pe flagParseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a flag parse error, got %v", err)
	}
	if stdout != "" || !strings.Contains(stderr, "no-such-flag") || !strings.Contains(stderr, "Usage:") {
		t.Errorf("flag error and usage belong on stderr; stdout=%q stderr=%q", stdout, stderr)
	}
}

// The helper process below runs the real Run entry point so the test can
// observe the actual exit code and log output of a refused launch. Only
// refusals are exercised: an allowed launch would serve forever.
const startupHelperEnv = "MARBOR_AGENT_STARTUP_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(startupHelperEnv) == "1" {
		Run(os.Args[1:], "test")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStartupProcessExitsNonZeroOnRefusal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--port=0", "--bind=0.0.0.0")
	cmd.Env = append(os.Environ(), startupHelperEnv+"=1", "MARBOR_AGENT_SECRET=test-token")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v output=%s", err, out)
	}
	if !strings.Contains(string(out), "refusing to serve plaintext HTTP on 0.0.0.0") {
		t.Errorf("output lacks the refusal message: %s", out)
	}
}
