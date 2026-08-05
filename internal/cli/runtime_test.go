package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_RuntimeRestart_JSON(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "restart", "gpu-0", "--server", srv.URL, "--token", "tok", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/runtime/restart" {
		t.Errorf("expected /admin/nodes/gpu-0/runtime/restart, got %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("expected Bearer tok, got %q", gotAuth)
	}
	if !strings.Contains(stdout.String(), `"ok": true`) {
		t.Errorf("expected ok:true in JSON output, got %s", stdout.String())
	}
}

func TestRun_RuntimeStart_Unconfigured_ExitUserError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"Runtime control unavailable: no control driver configured"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "start", "gpu-0", "--server", srv.URL, "--token", "tok"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no control driver configured") {
		t.Errorf("expected the mandated error message in stderr, got %q", stderr.String())
	}
}

func TestRun_RuntimeStop_ServerUnreachable_ExitServerError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "stop", "gpu-0", "--server", "http://127.0.0.1:1", "--token", "tok"}, &stdout, &stderr)
	if code != ExitServerError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitServerError, code, stderr.String())
	}
}

func TestRun_RuntimeAction_MissingCredentials_ExitUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "restart", "gpu-0"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
}

func TestRun_RuntimeAction_UnknownAction_ExitUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "bogus", "gpu-0"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d", ExitUserError, code)
	}
}

func TestRun_RuntimeAction_MissingNodeArg_ExitUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "restart"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d", ExitUserError, code)
	}
}

func TestRun_RuntimeLogs_JSON(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"lines":["line one","line two"]}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "logs", "gpu-0", "--lines", "50", "--server", srv.URL, "--token", "tok", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/runtime/logs?lines=50" {
		t.Errorf("expected /admin/nodes/gpu-0/runtime/logs?lines=50, got %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("expected Bearer tok, got %q", gotAuth)
	}
	if !strings.Contains(stdout.String(), "line one") || !strings.Contains(stdout.String(), "line two") {
		t.Errorf("expected both log lines in JSON output, got %s", stdout.String())
	}
}

func TestRun_RuntimeLogs_TextOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"lines":["line one","line two"]}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "logs", "gpu-0", "--server", srv.URL, "--token", "tok"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if stdout.String() != "line one\nline two\n" {
		t.Errorf("expected raw log lines, got %q", stdout.String())
	}
}

func TestRun_RuntimeLogs_NotSupported_ExitServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"process: log retrieval not supported without a supervisor"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "logs", "gpu-0", "--server", srv.URL, "--token", "tok"}, &stdout, &stderr)
	if code != ExitServerError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitServerError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not supported without a supervisor") {
		t.Errorf("expected the driver's real error text in stderr, got %q", stderr.String())
	}
}

func TestRun_RuntimeDrain_JSON(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu-0","draining":true,"reason":"maintenance"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "drain", "gpu-0", "--reason", "maintenance", "--server", srv.URL, "--token", "tok", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/drain" {
		t.Errorf("expected /admin/nodes/gpu-0/drain, got %s", gotPath)
	}
	if !strings.Contains(string(gotBody), `"reason":"maintenance"`) {
		t.Errorf("expected reason in request body, got %s", gotBody)
	}
	if !strings.Contains(stdout.String(), `"draining": true`) {
		t.Errorf("expected draining:true in JSON output, got %s", stdout.String())
	}
}

func TestRun_RuntimeUndrain_TextOutput(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu-0","draining":false}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "undrain", "gpu-0", "--server", srv.URL, "--token", "tok"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/drain" {
		t.Errorf("expected /admin/nodes/gpu-0/drain, got %s", gotPath)
	}
	if stdout.String() != "gpu-0: undrained\n" {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}
}

func TestRun_RuntimeHealth_Unhealthy_StillExitOK(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":false,"error":"connection refused","latencyMs":5}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "health", "gpu-0", "--server", srv.URL, "--token", "tok"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d (a completed probe reporting unhealthy is not a CLI failure), got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/health-check" {
		t.Errorf("expected /admin/nodes/gpu-0/health-check, got %s", gotPath)
	}
	if !strings.Contains(stdout.String(), "unhealthy - connection refused") {
		t.Errorf("expected unhealthy reason in stdout, got %q", stdout.String())
	}
}

func TestRun_RuntimeAction_Unauthorized_ExitAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"runtime", "restart", "gpu-0", "--server", srv.URL, "--token", "bad-token"}, &stdout, &stderr)
	if code != ExitAuthError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitAuthError, code, stderr.String())
	}
}
