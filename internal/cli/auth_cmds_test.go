package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// withTempConfigDir redirects userConfigDir to a fresh temp directory for
// the duration of a test, so session-file tests never touch the real OS
// config dir and never see a session left over from a previous test.
func withTempConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	orig := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userConfigDir = orig })
}

func TestSessionFile_SaveLoadDelete(t *testing.T) {
	withTempConfigDir(t)

	if s, err := loadSession(); err != nil || s != nil {
		t.Fatalf("expected no saved session initially, got %+v, err=%v", s, err)
	}

	want := savedSession{Server: "http://localhost:8080", Token: "tok-123", Username: "admin", Role: "admin"}
	if err := saveSession(want); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	path, err := sessionFilePath()
	if err != nil {
		t.Fatalf("sessionFilePath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat session file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("expected session file mode 0600, got %o", perm)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if strings.Contains(string(data), "expires_at") {
		t.Errorf("saved session file must never carry an expiry timestamp, got %s", data)
	}

	got, err := loadSession()
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("loadSession round-trip mismatch: got %+v, want %+v", got, want)
	}

	if err := deleteSession(); err != nil {
		t.Fatalf("deleteSession: %v", err)
	}
	if s, err := loadSession(); err != nil || s != nil {
		t.Fatalf("expected no saved session after delete, got %+v, err=%v", s, err)
	}
	// Idempotent: deleting again is not an error.
	if err := deleteSession(); err != nil {
		t.Fatalf("second deleteSession should be a no-op, got %v", err)
	}
}

func TestAuthenticatedClient_FallbackToSavedSession(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/nodes" && r.Header.Get("Authorization") == "Bearer saved-tok" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "saved-tok", Username: "admin", Role: "admin"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d using the saved session with zero flags, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
}

func TestAuthenticatedClient_SavedSessionIgnoredForDifferentServer(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	// Session saved against a different server entirely - must never be
	// replayed against srv.URL.
	if err := saveSession(savedSession{Server: "http://other-mesh.invalid", Token: "saved-tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d (missing credentials) since the saved session is for a different server, got %d", ExitUserError, code)
	}
}

func TestAuthenticatedClient_ExplicitTokenBeatsSavedSession(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer explicit-tok" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "saved-tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL, "--token", "explicit-tok", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, --token must win over the saved session, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
}

func TestRun_Login_WithFlags_SavesSessionWithoutLeakingToken(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/login" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		http.SetCookie(w, &http.Cookie{Name: "mesh_session", Value: "super-secret-token"})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"role":"admin","username":"admin","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"login", "--server", srv.URL, "--username", "admin", "--password", "admin"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if strings.Contains(stdout.String(), "super-secret-token") || strings.Contains(stderr.String(), "super-secret-token") {
		t.Fatalf("login must never print the session token, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	session, err := loadSession()
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if session == nil {
		t.Fatal("expected a saved session after login")
	}
	if session.Token != "super-secret-token" || session.Username != "admin" || session.Role != "admin" || session.Server != srv.URL {
		t.Fatalf("unexpected saved session: %+v", session)
	}
}

func TestRun_Login_NonInteractive_NoCredentials_UserError(t *testing.T) {
	withTempConfigDir(t)

	var stdout, stderr bytes.Buffer
	// No --username/--password/--token, and stdin (whatever it is in a test
	// process) is not expected to be a terminal - either way this must not
	// hang waiting for input.
	code := Run([]string{"login", "--server", "http://example.invalid"}, &stdout, &stderr)
	if code != ExitUserError && code != ExitOK {
		// If the test runner's stdin happens to be a terminal, the
		// interactive path could take over - guard against that being
		// misread as a hang rather than assert a single fixed exit code.
		t.Fatalf("expected a prompt-or-error path, got exit %d (stderr: %s)", code, stderr.String())
	}
}

func TestRun_Logout_RemovesSessionAndIsIdempotent(t *testing.T) {
	withTempConfigDir(t)

	if err := saveSession(savedSession{Server: "http://localhost:8080", Token: "tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logout"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if s, _ := loadSession(); s != nil {
		t.Fatalf("expected session to be removed, got %+v", s)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"logout"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("logout must be idempotent - expected exit %d on an already-logged-out session, got %d", ExitOK, code)
	}
}

func TestRun_Whoami_NotLoggedIn(t *testing.T) {
	withTempConfigDir(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"whoami"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
}

func TestRun_Whoami_Active(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/nodes" && r.Header.Get("Authorization") == "Bearer good-tok" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "good-tok", Username: "admin", Role: "admin"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"whoami", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out whoamiOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("could not parse JSON output: %v (%s)", err, stdout.String())
	}
	if out.Cached || out.Status != "active" || out.Username != "admin" {
		t.Fatalf("expected a live-verified active identity, got %+v", out)
	}
}

func TestRun_Whoami_ExpiredSession(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"session expired"}`))
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "stale-tok", Username: "admin", Role: "admin"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"whoami", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitAuthError {
		t.Fatalf("expected exit %d for an expired/invalid saved session, got %d (stderr: %s)", ExitAuthError, code, stderr.String())
	}
	var out whoamiOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("could not parse JSON output: %v (%s)", err, stdout.String())
	}
	if !out.Cached || out.Username != "admin" {
		t.Fatalf("expected the cached identity marked cached=true, got %+v", out)
	}
}

func TestRun_Whoami_ServerUnreachable_ShowsCachedIdentity(t *testing.T) {
	withTempConfigDir(t)

	if err := saveSession(savedSession{Server: "http://127.0.0.1:1", Token: "tok", Username: "admin", Role: "admin"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"whoami", "--server", "http://127.0.0.1:1", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("an unreachable server must not hard-fail whoami - expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out whoamiOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("could not parse JSON output: %v (%s)", err, stdout.String())
	}
	if !out.Cached || out.Username != "admin" {
		t.Fatalf("expected cached identity shown despite being unreachable, got %+v", out)
	}
}
