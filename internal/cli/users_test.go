package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_UsersList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":1,"username":"bob","email":"bob@example.com","role":"user","status":"active","api_key_name":"bob-key","created_at":"2026-08-29T00:00:00Z"}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "list", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bob") {
		t.Fatalf("missing bob %q", stdout.String())
	}
}

func TestRun_UsersCreate_SendsBody(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":42,"username":"alice","role":"user","status":"pending","initial_password":"secret123"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "create", "--user", "alice", "--role", "user", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotBody["username"] != "alice" {
		t.Fatalf("body %v", gotBody)
	}
	if !strings.Contains(stdout.String(), "secret123") {
		t.Fatalf("should print password %q", stdout.String())
	}
}

func TestRun_UsersApprove_Basic(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"user":{"id":5,"username":"bob","status":"active"},"api_key_value":"sk-abc"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "approve", "5", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotPath != "/admin/v1/users/5/approve" {
		t.Fatalf("path %q", gotPath)
	}
	if !strings.Contains(stdout.String(), "5 approved") {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestRun_UsersApprove_WithCreateKey(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"user":{"id":5,"username":"bob","status":"active"},"api_key_value":"sk-xyz"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "approve", "5", "--api-key-name", "bob-key", "--create-key", "--key-rate-limit", "10", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotBody["create_key"] == nil {
		t.Fatalf("expected create_key %v", gotBody)
	}
}

func TestRun_UsersSuspend_Yes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "suspend", "7", "--yes", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotPath != "/admin/v1/users/7/suspend" {
		t.Fatalf("path %q", gotPath)
	}
}

func TestRun_UsersSuspend_NoTTYAborts(t *testing.T) {
	orig := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = orig }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not be called")
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "suspend", "7", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected abort %d %q", code, stderr.String())
	}
}

func TestRun_UsersResetPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"initial_password":"newpass123"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "reset-password", "9", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "newpass123") {
		t.Fatalf("missing password %q", stdout.String())
	}
}

func TestRun_UsersPatch_RequiresField(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "patch", "1", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected error %d %q", code, stderr.String())
	}
}

func TestRun_UsersPatch_SendsEmail(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":1,"username":"bob","email":"new@example.com","role":"user","status":"active"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "patch", "1", "--email", "new@example.com", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotBody["email"] != "new@example.com" {
		t.Fatalf("body %v", gotBody)
	}
}

func TestRun_UsersDelete_Yes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "delete", "11", "--yes", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if gotPath != "/admin/v1/users/11" {
		t.Fatalf("path %q", gotPath)
	}
}

func TestRun_UsersDelete_NoTTYAborts(t *testing.T) {
	orig := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = orig }()
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"users", "delete", "11", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected abort %d %q", code, stderr.String())
	}
}
