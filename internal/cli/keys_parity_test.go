package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_KeyList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/v1/keys" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"finance","key":"sk-****","rateLimit":100,"requestsToday":1,"requestsThisMonth":2,"allowedModels":["llama3"],"status":"active"}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "list", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "finance") || !strings.Contains(stdout.String(), "sk-****") {
		t.Fatalf("table missing data %q", stdout.String())
	}
}

func TestRun_KeyList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "list", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("not json %q", stdout.String())
	}
}

func TestRun_KeyCreate_SendsBody(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/v1/keys" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"newkey","key":"sk-plaintext123","rateLimit":10}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "create", "--name", "newkey", "--rate-limit", "10", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d stderr %q", code, stderr.String())
	}
	if gotBody["name"] != "newkey" {
		t.Fatalf("body name %v", gotBody)
	}
	if !strings.Contains(stdout.String(), "sk-plaintext123") {
		t.Fatalf("stdout should contain plaintext %q", stdout.String())
	}
}

func TestRun_KeyCreate_MissingName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "create", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected user error got %d %q", code, stderr.String())
	}
}

func TestRun_KeyRevoke_Yes(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "revoke", "finance", "--yes", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/admin/v1/keys/finance" {
		t.Fatalf("unexpected %s %s", gotMethod, gotPath)
	}
}

func TestRun_KeyRevoke_NonTTYWithoutYes_Aborts(t *testing.T) {
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
	code := Run([]string{"key", "revoke", "finance", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected abort got %d %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("should mention --yes %q", stderr.String())
	}
}

func TestRun_KeyRevoke_InteractiveYes(t *testing.T) {
	origTTY := stdinIsTTY
	origReader := stdinReader
	stdinIsTTY = func() bool { return true }
	stdinReader = strings.NewReader("y\n")
	defer func() { stdinIsTTY = origTTY; stdinReader = origReader }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "revoke", "finance", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected OK got %d %q", code, stderr.String())
	}
}

func TestRun_KeyPatch_Empty_Aborts(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "patch", "finance", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected user error got %d %q", code, stderr.String())
	}
}

func TestRun_KeyPatch_SendsOnlyVisited(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "patch", "finance", "--rate-limit", "5", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotBody["rate_limit"] == nil {
		t.Fatalf("expected rate_limit in body %v", gotBody)
	}
	if gotBody["daily_limit"] != nil {
		t.Fatalf("should not have daily_limit %v", gotBody)
	}
}

func TestRun_KeyList_MissingCredentials(t *testing.T) {
	withTempConfigDir(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "list", "--server", "http://example.invalid"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected ExitUserError got %d %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "marbor login") {
		t.Fatalf("missing hint %q", stderr.String())
	}
}
