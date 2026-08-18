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

// TestMain redirects userConfigDir to a temp directory for this entire test
// binary run, before any test executes - a package-wide safety net so that a
// test which forgets withTempConfigDir (below) still can't read or write the
// real OS config dir or trip over a session a developer has actually logged
// into. This matters concretely for authenticatedClient's saved-session
// fallback: a test built around "no credentials given" (e.g. in
// runtime_test.go/models_test.go/control_test.go, none of which know
// anything about sessions) would otherwise silently pick up a real saved
// session on any machine where someone has run `ollama-mesh login`, and -
// since the credentialed commands are mutating (runtime restart, model
// pull/delete) - could fire a real request against whatever --server those
// tests use.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ollama-mesh-cli-test-config-*")
	if err != nil {
		panic(err)
	}
	userConfigDir = func() (string, error) { return dir, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// withTempConfigDir redirects userConfigDir to its own fresh temp directory
// for the duration of a test, on top of TestMain's package-wide one - tests
// that read/write/assert on session-file state need isolation from every
// other test, not just from the real OS config dir.
func withTempConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	orig := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userConfigDir = orig })
}

// mustSaveSession saves a session for server/token, failing the test on
// error. Used by control_test.go/models_test.go/runtime_test.go as the
// stand-in for what those tests used to do with a bare --token flag value -
// there is no CLI flag for handing in an existing token anymore (see
// newFlagSet's doc comment in cli.go), so tests that only need
// authenticatedClient to succeed (not to exercise login itself) go through
// the saved-session file directly. Callers must call withTempConfigDir(t)
// first for isolation from other tests.
func mustSaveSession(t *testing.T, server, token string) {
	t.Helper()
	if err := saveSession(savedSession{Server: server, Token: token}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}
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

func TestAuthenticatedClient_ExplicitUsernamePasswordBeatsSavedSession(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "mesh_session", Value: "explicit-tok"})
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"role":"admin","username":"admin"}`))
		case "/admin/v1/nodes":
			if r.Header.Get("Authorization") == "Bearer explicit-tok" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[]`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "saved-tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL, "--username", "admin", "--password", "admin", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, --username/--password must win over the saved session, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
}

func TestRun_Login_NoTokenFlag_IsUnknownFlag(t *testing.T) {
	withTempConfigDir(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"login", "--server", "http://example.invalid", "--token", "anything"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d (unknown flag), got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("expected an unknown-flag error for --token, got %q", stderr.String())
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

func TestRun_Login_UsernameFlagPlusPasswordEnv_BothResolve(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Password string }
		json.NewDecoder(r.Body).Decode(&body)
		if body.Username != "admin" || body.Password != "from-env" {
			t.Fatalf("expected username/password resolved from mixed flag+env sources, got %+v", body)
		}
		http.SetCookie(w, &http.Cookie{Name: "mesh_session", Value: "tok"})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"role":"admin","username":"admin"}`))
	}))
	defer srv.Close()

	t.Setenv("MESH_PASSWORD", "from-env")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"login", "--server", srv.URL, "--username", "admin"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
}

func TestRun_NoEnvSecretLeaksViaHelpOrParseError(t *testing.T) {
	withTempConfigDir(t)
	t.Setenv("MESH_PASSWORD", "SEKRET-PASSWORD-VALUE")

	for _, args := range [][]string{
		{"whoami", "--help"},
		{"login", "--help"},
		{"logout", "--help"},
		{"nodes", "--help"},
		{"version", "--help"},
		{"status", "--help"},
		{"nodes", "--this-flag-does-not-exist"},
		{"login", "--this-flag-does-not-exist"},
	} {
		var stdout, stderr bytes.Buffer
		Run(args, &stdout, &stderr)
		for _, secret := range []string{"SEKRET-PASSWORD-VALUE"} {
			if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
				t.Fatalf("%v leaked %q - stdout=%q stderr=%q", args, secret, stdout.String(), stderr.String())
			}
		}
	}
}

func TestAuthenticatedClient_SavedSessionMatchesDespiteTrailingSlash(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer saved-tok" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	// Saved with a trailing slash (e.g. from a prior `login --server X/`).
	if err := saveSession(savedSession{Server: srv.URL + "/", Token: "saved-tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	// Looked up WITHOUT a trailing slash - must still match.
	code := Run([]string{"nodes", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d despite the trailing-slash mismatch, got %d (stderr: %s)", ExitOK, code, stderr.String())
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

func TestRun_Logout_CallsServerLogoutAndDeletesSession(t *testing.T) {
	withTempConfigDir(t)

	var gotMethod, gotPath, gotAuth string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logout", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("expected exactly one server call, got %d", calls)
	}
	if gotMethod != http.MethodPost || gotPath != "/logout" {
		t.Errorf("expected POST /logout, got %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("expected Bearer tok, got %q", gotAuth)
	}
	if s, _ := loadSession(); s != nil {
		t.Fatalf("expected local session to be removed, got %+v", s)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output did not parse: %v (%s)", err, stdout.String())
	}
	if out["ok"] != true || out["server_logout"] != true {
		t.Errorf("unexpected JSON output: %+v", out)
	}
}

func TestRun_Logout_ServerCallFails_StillDeletesSessionAndExitsOK(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	if err := saveSession(savedSession{Server: srv.URL, Token: "tok"}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logout"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("a failed server-side logout must not fail the command - expected exit %d, got %d", ExitOK, code)
	}
	if s, _ := loadSession(); s != nil {
		t.Fatalf("expected local session to be removed despite the server failure, got %+v", s)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("expected a warning on stderr about the failed server-side logout, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "warning") {
		t.Errorf("warning must go to stderr, not stdout, got %q", stdout.String())
	}
}

func TestRun_Logout_NoSession_NoOpAndNoHTTPCall(t *testing.T) {
	withTempConfigDir(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logout"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("logout with no existing session must still be a no-op success - expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if calls != 0 {
		t.Fatalf("expected zero HTTP calls when there is no session to log out of, got %d", calls)
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
