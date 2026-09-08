package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_SettingsGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/settings" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"timezone":"Local","webhook":{"secret":"***"}}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"settings", "get", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"secret":"***"`) {
		t.Errorf("expected masked secret in output, got %s", stdout.String())
	}
}

func TestRun_SettingsSet(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/admin/settings" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(file, []byte(`{"timezone":"America/New_York"}`), 0o600); err != nil {
		t.Fatalf("write settings file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"settings", "set", "--file", file, "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("request body not valid JSON: %v (%s)", err, gotBody)
	}
	if got["timezone"] != "America/New_York" {
		t.Errorf("timezone = %v, want America/New_York", got["timezone"])
	}
	if !strings.Contains(stdout.String(), "settings updated") {
		t.Errorf("expected confirmation message, got %s", stdout.String())
	}
}

func TestRun_SettingsSet_MissingFile(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://example.invalid", "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"settings", "set", "--file", filepath.Join(t.TempDir(), "does-not-exist.json")}, &stdout, &stderr)
	if code == ExitOK {
		t.Fatalf("expected non-zero exit for missing file, got %d", code)
	}
}

func TestRun_SettingsSet_MissingFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"settings", "set"}, &stdout, &stderr)
	if code == ExitOK {
		t.Fatalf("expected non-zero exit for missing --file, got %d", code)
	}
	if !strings.Contains(stderr.String(), "--file is required") {
		t.Errorf("expected required-flag error, got %s", stderr.String())
	}
}
