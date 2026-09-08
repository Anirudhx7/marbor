package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_ModelsPull_JSON(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"ok":true,"node":"gpu-0","model":"llama3:8b"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "pull", "gpu-0", "llama3:8b", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/pull" {
		t.Errorf("expected /admin/nodes/gpu-0/pull, got %s", gotPath)
	}
	if !strings.Contains(string(gotBody), `"model":"llama3:8b"`) {
		t.Errorf("expected model in request body, got %s", gotBody)
	}
	if !strings.Contains(stdout.String(), `"model": "llama3:8b"`) {
		t.Errorf("expected model in JSON output, got %s", stdout.String())
	}
}

func TestRun_ModelsDelete_TextOutput(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "delete", "gpu-0", "llama3:8b", "--yes", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/models/llama3:8b" {
		t.Errorf("expected /admin/nodes/gpu-0/models/llama3:8b, got %s", gotPath)
	}
	if stdout.String() != "gpu-0: deleted llama3:8b\n" {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}
}

// TestRun_ModelsDelete_NonTTYWithoutYes_Aborts mirrors
// TestRun_KeyRevoke_NonTTYWithoutYes_Aborts (keys_parity_test.go) - models
// delete is storage-destructive and irreversible, so it must refuse without
// --yes when there is no TTY to confirm, and never contact the server.
func TestRun_ModelsDelete_NonTTYWithoutYes_Aborts(t *testing.T) {
	origTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = origTTY }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called")
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "delete", "gpu-0", "llama3:8b", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected abort got %d %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("should mention --yes %q", stderr.String())
	}
}

// TestRun_ModelsDelete_InteractiveYes mirrors TestRun_KeyRevoke_InteractiveYes.
func TestRun_ModelsDelete_InteractiveYes(t *testing.T) {
	origTTY := stdinIsTTY
	origReader := stdinReader
	stdinIsTTY = func() bool { return true }
	stdinReader = strings.NewReader("y\n")
	defer func() { stdinIsTTY = origTTY; stdinReader = origReader }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "delete", "gpu-0", "llama3:8b", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected OK got %d %q", code, stderr.String())
	}
}

func TestRun_ModelsUnload_TextOutput(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "unload", "gpu-0", "llama3:8b", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/unload" {
		t.Errorf("expected /admin/nodes/gpu-0/unload, got %s", gotPath)
	}
	if stdout.String() != "gpu-0: unloaded llama3:8b\n" {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}
}

func TestRun_ModelsList_JSON(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3:8b","sizeBytes":4700000000,"source":"ollama-tags","family":"llama"}]}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "list", "gpu-0", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/models" {
		t.Errorf("expected /admin/nodes/gpu-0/models, got %s", gotPath)
	}
	if !strings.Contains(stdout.String(), `"name": "llama3:8b"`) {
		t.Errorf("expected model name in JSON output, got %s", stdout.String())
	}
}

func TestRun_ModelsPull_MissingModelArg_ExitUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "pull", "gpu-0"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d", ExitUserError, code)
	}
}

func TestRun_Models_BareCommand_UnaffectedByNewSubcommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/models" {
			t.Errorf("expected fleet-wide /admin/v1/models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
}

// TestRun_Models_UnknownAction_Errors guards against the models subcommand
// switch silently falling through to the bare fleet-wide list on a typo'd
// action - it must reject an unrecognized action the same way runtime/node
// control already do, not silently run the wrong command.
func TestRun_Models_UnknownAction_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be contacted for an unknown models action, got %s", r.URL.Path)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "bogus", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stdout: %s, stderr: %s)", ExitUserError, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown models action") {
		t.Fatalf("expected an unknown-action error on stderr, got: %s", stderr.String())
	}
}
