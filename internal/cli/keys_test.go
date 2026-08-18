package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_KeySetLocalOnly(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-local-only", "finance", "true", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/keys/finance" {
		t.Errorf("expected /admin/v1/keys/finance, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"local_only":true`) {
		t.Errorf("expected body to contain local_only:true, got %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "finance") || !strings.Contains(stdout.String(), "true") {
		t.Errorf("expected confirmation mentioning finance/true, got %q", stdout.String())
	}
}

func TestRun_KeySetLocalOnly_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-local-only", "finance", "true", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v (%s)", err, stdout.String())
	}
	if out["key"] != "finance" || out["local_only"] != true || out["ok"] != true {
		t.Errorf("unexpected JSON output: %+v", out)
	}
}

func TestRun_KeySetLocalOnly_InvalidValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-local-only", "finance", "yes"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid value") {
		t.Errorf("expected an invalid-value error, got %q", stderr.String())
	}
}

func TestRun_KeySetAllowLocalDegradation(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-allow-local-degradation", "finance", "true", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/keys/finance" {
		t.Errorf("expected /admin/v1/keys/finance, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"allow_local_degradation":true`) {
		t.Errorf("expected body to contain allow_local_degradation:true, got %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "finance") || !strings.Contains(stdout.String(), "true") {
		t.Errorf("expected confirmation mentioning finance/true, got %q", stdout.String())
	}
}

func TestRun_KeySetAllowLocalDegradation_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-allow-local-degradation", "finance", "true", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v (%s)", err, stdout.String())
	}
	if out["key"] != "finance" || out["allow_local_degradation"] != true || out["ok"] != true {
		t.Errorf("unexpected JSON output: %+v", out)
	}
}

func TestRun_KeySetAllowLocalDegradation_InvalidValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-allow-local-degradation", "finance", "yes"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid value") {
		t.Errorf("expected an invalid-value error, got %q", stderr.String())
	}
}

func TestRun_Spill_JSON(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"key_name":"finance","served_by":"local","requests":5}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"spill", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/spill" {
		t.Errorf("expected /admin/v1/spill, got %s", gotPath)
	}
	if !strings.Contains(stdout.String(), "finance") {
		t.Errorf("expected finance in JSON output, got %s", stdout.String())
	}
}

func TestRun_Spill_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"key_name":"finance","served_by":"blocked","requests":2}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"spill", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "KEY") || !strings.Contains(out, "SERVED BY") || !strings.Contains(out, "REQUESTS") {
		t.Errorf("expected a table header, got %q", out)
	}
	if !strings.Contains(out, "finance") || !strings.Contains(out, "blocked") {
		t.Errorf("expected row data, got %q", out)
	}
}
