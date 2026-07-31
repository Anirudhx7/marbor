package cli

import (
	"bytes"
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
