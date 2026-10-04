package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// bootEnv is a fresh database plus the data directory next to it.
type bootEnv struct {
	st  store.Store
	dir string
	r   *router.Router
}

func newBootEnv(t *testing.T) bootEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "marbor.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return bootEnv{st: st, dir: dir, r: router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)}
}

// boot starts a server over the environment with the given process env.
func (e bootEnv) boot(env map[string]string) *Server {
	return NewServerWithBootstrap(e.r, nil, config.Config{}, e.st, BootstrapOptions{DataDir: e.dir, Getenv: envFrom(env)})
}

func (e bootEnv) pwFile() string { return bootstrapcred.FilePath(e.dir) }

func (e bootEnv) marker(t *testing.T) *bootstrapcred.Marker {
	t.Helper()
	raw, _ := e.st.GetSetting(bootstrapcred.SettingKey)
	m, err := bootstrapcred.DecodeMarker(raw)
	if err != nil {
		t.Fatalf("DecodeMarker: %v", err)
	}
	return m
}

func (e bootEnv) addAdmin(t *testing.T, username, password string, mustChange bool, skips int) {
	t.Helper()
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	u := store.User{Username: username, Role: "admin", Status: "active", PasswordHash: hash, MustChangePassword: mustChange, CreatedAt: time.Now()}
	id, err := e.st.CreateUser(u)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u.ID, u.SkipPasswordCount = id, skips
	if err := e.st.UpdateUser(u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
}

func TestFreshBoot_GeneratesPrivatePasswordFile(t *testing.T) {
	// Arrange
	e := newBootEnv(t)
	logs := captureLog(t)

	// Act
	s := e.boot(nil)

	// Assert
	pw, err := bootstrapcred.ReadSafe(e.pwFile())
	if err != nil {
		t.Fatalf("password file: %v", err)
	}
	if len(pw) < 24 || pw == defaultAdminPassword {
		t.Fatalf("generated password length %d / default=%v", len(pw), pw == defaultAdminPassword)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(e.pwFile()); info.Mode().Perm() != 0o600 {
			t.Errorf("password file mode = %v, want 0600", info.Mode().Perm())
		}
	}
	u, err := e.st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if !u.MustChangePassword || u.SkipPasswordCount < maxSkipPasswordChanges {
		t.Errorf("must_change=%v skips=%d; want forced change with skipping disabled", u.MustChangePassword, u.SkipPasswordCount)
	}
	if verifyPassword(u.PasswordHash, defaultAdminPassword) || !verifyPassword(u.PasswordHash, pw) {
		t.Error("admin must accept only the generated password, never admin")
	}
	if m := e.marker(t); m == nil || m.Origin != bootstrapcred.OriginFresh || m.Username != "admin" {
		t.Errorf("marker = %+v, want fresh/admin", m)
	}
	if !s.BootstrapPasswordPending() {
		t.Error("pending flag not set after a fresh boot")
	}
	hint := s.BootstrapLoginHint()
	if !strings.Contains(hint, e.pwFile()) || strings.Contains(hint, pw) {
		t.Errorf("banner hint must name the path and never the password: %q", hint)
	}
	if strings.Contains(logs.String(), pw) {
		t.Error("the generated password appeared in the log output")
	}
}

func TestFreshBoot_PasswordDiffersPerInstall(t *testing.T) {
	a, b := newBootEnv(t), newBootEnv(t)
	a.boot(nil)
	b.boot(nil)

	pa, _ := bootstrapcred.ReadSafe(a.pwFile())
	pb, _ := bootstrapcred.ReadSafe(b.pwFile())

	if pa == "" || pa == pb {
		t.Errorf("passwords must be random and distinct, got %q and %q", pa, pb)
	}
}

func TestFreshBoot_OperatorSuppliedSecret(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "run-secret")
	if err := os.WriteFile(secretFile, []byte("Supplied-Via-File-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"password file", map[string]string{bootstrapcred.EnvPasswordFile: secretFile}, "Supplied-Via-File-1"},
		{"plain env", map[string]string{bootstrapcred.EnvPassword: "Supplied-Via-Env-1"}, "Supplied-Via-Env-1"},
		{"file wins over env", map[string]string{bootstrapcred.EnvPasswordFile: secretFile, bootstrapcred.EnvPassword: "ignored-value"}, "Supplied-Via-File-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			logs := captureLog(t)

			s := e.boot(tt.env)

			u, err := e.st.GetUserByUsername("admin")
			if err != nil || !verifyPassword(u.PasswordHash, tt.want) {
				t.Fatalf("admin does not accept the supplied password: %v", err)
			}
			if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
				t.Error("a generated file was written although the operator supplied the secret")
			}
			if m := e.marker(t); m == nil || m.Origin != bootstrapcred.OriginFresh {
				t.Errorf("marker = %+v, want fresh", m)
			}
			if s.boot.err != nil || strings.Contains(logs.String(), tt.want) {
				t.Errorf("boot err=%v or the secret leaked into the log", s.boot.err)
			}
		})
	}
}

func TestFreshBoot_RejectsUnusableSuppliedSecret(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"public default":  {bootstrapcred.EnvPassword: "admin"},
		"unreadable file": {bootstrapcred.EnvPasswordFile: filepath.Join(t.TempDir(), "absent")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newBootEnv(t)

			s := e.boot(env)

			if _, err := e.st.GetUserByUsername("admin"); err == nil {
				t.Error("an admin was created on an unusable supplied secret")
			}
			if s.boot.err == nil {
				t.Error("boot must fail closed")
			}
			if e.marker(t) != nil {
				t.Error("no marker may be written when the secret was unusable")
			}
		})
	}
}

func TestFreshBoot_FailsClosedWhenFileCannotBeUsed(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, e *bootEnv)
	}{
		{"data directory missing", func(t *testing.T, e *bootEnv) { e.dir = filepath.Join(e.dir, "does", "not", "exist") }},
		{"existing file is a symlink", func(t *testing.T, e *bootEnv) {
			target := filepath.Join(e.dir, "elsewhere")
			if err := os.WriteFile(target, []byte("planted\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, e.pwFile()); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
		{"existing file has loose permissions", func(t *testing.T, e *bootEnv) {
			if runtime.GOOS == "windows" {
				t.Skip("unix permission bits")
			}
			if err := os.WriteFile(e.pwFile(), []byte("planted\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(e.pwFile(), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			tt.setup(t, &e)

			s := e.boot(nil)

			if _, err := e.st.GetUserByUsername("admin"); err == nil {
				t.Error("an admin was created although the password file could not be used")
			}
			if s.boot.err == nil {
				t.Fatal("boot must record the failure")
			}
			if e.marker(t) != nil {
				t.Error("no marker may be written on a failed boot")
			}
			gate := s.AdminListenerGate(newFakeListener("127.0.0.1"), "127.0.0.1:8080")
			if gate == nil || !strings.Contains(gate.Error(), "first-boot") {
				t.Errorf("gate = %v, want a refusal naming the first-boot credential", gate)
			}
		})
	}
}

func TestFreshBoot_FailedBootIsRetryableAndKeepsSamePassword(t *testing.T) {
	// A previous boot wrote the file and crashed before the user row existed.
	e := newBootEnv(t)
	if err := bootstrapcred.WriteNew(e.pwFile(), "Leftover-From-Crash-1"); err != nil {
		t.Fatal(err)
	}

	e.boot(nil)

	u, err := e.st.GetUserByUsername("admin")
	if err != nil || !verifyPassword(u.PasswordHash, "Leftover-From-Crash-1") {
		t.Fatalf("retry must reuse the file's password: %v", err)
	}
}

func TestRestartWhilePending_KeepsSameFileAndPassword(t *testing.T) {
	e := newBootEnv(t)
	e.boot(nil)
	before, _ := os.ReadFile(e.pwFile())

	s := e.boot(nil)

	after, _ := os.ReadFile(e.pwFile())
	if !bytes.Equal(before, after) {
		t.Error("the password file changed across a restart")
	}
	if !s.BootstrapPasswordPending() || s.boot.err != nil {
		t.Errorf("pending=%v err=%v after restart", s.BootstrapPasswordPending(), s.boot.err)
	}
}

func TestRestart_FailsClosedOnUnsafeExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	e := newBootEnv(t)
	e.boot(nil)
	if err := os.Chmod(e.pwFile(), 0o666); err != nil {
		t.Fatal(err)
	}

	s := e.boot(nil)

	if s.boot.err == nil {
		t.Fatal("a world-writable password file must stop the admin listener")
	}
}

func TestPasswordChange_ClearsMarkerAndRemovesFile(t *testing.T) {
	// Arrange
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	handler := s.Handler()
	login := postJSON(handler, "/admin/login", map[string]string{"username": "admin", "password": pw}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login with the generated password = %d: %s", login.Code, login.Body.String())
	}
	cookie := sessionCookieOf(login)

	// Act
	change := postJSON(handler, "/admin/change-password", map[string]string{"new_password": "Replacement-Passw0rd-9"}, cookie)

	// Assert
	if change.Code != http.StatusOK {
		t.Fatalf("change-password = %d: %s", change.Code, change.Body.String())
	}
	if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
		t.Error("password file still present after the change")
	}
	if e.marker(t) != nil || s.BootstrapPasswordPending() {
		t.Error("marker or pending flag survived the password change")
	}
	if gate := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080"); gate != nil {
		t.Errorf("a non-loopback bind must be allowed once the password is changed, got %v", gate)
	}
}

func TestPasswordChange_RemovalFailureStillClearsMarker(t *testing.T) {
	// Arrange: make the "file" a non-empty directory so unlinking it fails.
	e := newBootEnv(t)
	s := e.boot(map[string]string{bootstrapcred.EnvPassword: "Supplied-Via-Env-1"})
	if err := os.MkdirAll(filepath.Join(e.pwFile(), "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	// Act
	s.onPasswordChanged("admin")

	// Assert
	if e.marker(t) != nil {
		t.Error("marker must be cleared even though the file could not be removed")
	}
	if !strings.Contains(logs.String(), e.pwFile()) {
		t.Errorf("the removal failure must log the path: %s", logs.String())
	}
}

func TestBoot_RemovesStalePasswordFileWithWarning(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", "Already-Changed-Passw0rd", false, 0)
	if err := bootstrapcred.WriteNew(e.pwFile(), "stale-value"); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	e.boot(nil)

	if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
		t.Error("a stale password file survived the boot")
	}
	if !strings.Contains(logs.String(), "stale") || strings.Contains(logs.String(), "stale-value") {
		t.Errorf("want a stale-file warning without the contents: %s", logs.String())
	}
}

func TestMarker_TransferAndClearOnUserChanges(t *testing.T) {
	setup := func(t *testing.T, secondPassword string) (bootEnv, *Server) {
		e := newBootEnv(t)
		s := e.boot(map[string]string{bootstrapcred.EnvPassword: "Supplied-Via-Env-1"})
		e.addAdmin(t, "ops", secondPassword, true, maxSkipPasswordChanges)
		return e, s
	}
	removeMarked := map[string]func(e bootEnv){
		"delete": func(e bootEnv) { u, _ := e.st.GetUserByUsername("admin"); _ = e.st.SoftDeleteUser(u.ID, "test") },
		"suspend": func(e bootEnv) {
			u, _ := e.st.GetUserByUsername("admin")
			u.Status = "suspended"
			_ = e.st.UpdateUser(u)
		},
		"demote": func(e bootEnv) { u, _ := e.st.GetUserByUsername("admin"); u.Role = "user"; _ = e.st.UpdateUser(u) },
	}
	for action, apply := range removeMarked {
		t.Run(action+" transfers to another default-password admin", func(t *testing.T) {
			e, s := setup(t, defaultAdminPassword)
			created := e.marker(t).CreatedAt

			apply(e)
			s.onUserAccessChanged()

			m := e.marker(t)
			if m == nil || m.Username != "ops" || m.Origin != bootstrapcred.OriginFresh || !m.CreatedAt.Equal(created) {
				t.Errorf("marker = %+v, want transferred to ops with origin and created_at kept", m)
			}
			if !s.BootstrapPasswordPending() {
				t.Error("pending flag dropped although a default-password admin remains")
			}
		})
		t.Run(action+" clears when no default-password admin remains", func(t *testing.T) {
			e, s := setup(t, "Strong-Unrelated-Passw0rd")

			apply(e)
			s.onUserAccessChanged()

			if e.marker(t) != nil || s.BootstrapPasswordPending() {
				t.Error("marker must be cleared when no admin is on an initial password")
			}
		})
	}
	t.Run("marked user still an active admin keeps the marker", func(t *testing.T) {
		e, s := setup(t, defaultAdminPassword)
		s.onUserAccessChanged()
		if m := e.marker(t); m == nil || m.Username != "admin" {
			t.Errorf("marker = %+v, want unchanged", m)
		}
	})
}

func TestMarker_LostMarkerStillDefaultActive(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", defaultAdminPassword, true, 0)

	s := e.boot(nil)

	if !s.BootstrapPasswordPending() {
		t.Error("an admin on the public default must count as default-active without a marker")
	}
	if e.marker(t) == nil {
		t.Error("the safety net must record the marker on first boot")
	}
}

func TestClassifyExisting_OriginByRecoveryState(t *testing.T) {
	tests := []struct {
		name       string
		mustChange bool
		skips      int
		want       bootstrapcred.Origin
	}{
		{"recovery state: forced change and skip cap reached", true, maxSkipPasswordChanges, bootstrapcred.OriginLegacyRecovery},
		{"forced change but skips remain", true, 0, bootstrapcred.OriginUpgraded},
		{"no forced change", false, 0, bootstrapcred.OriginUpgraded},
		{"skip cap reached without forced change (cap alone decides)", false, maxSkipPasswordChanges, bootstrapcred.OriginLegacyRecovery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			e.addAdmin(t, "admin", defaultAdminPassword, tt.mustChange, tt.skips)
			logs := captureLog(t)

			e.boot(nil)

			m := e.marker(t)
			if m == nil || m.Origin != tt.want {
				t.Fatalf("marker = %+v, want origin %s", m, tt.want)
			}
			if !strings.Contains(logs.String(), string(tt.want)) {
				t.Errorf("the classification and its reason must be logged: %s", logs.String())
			}
			if strings.Contains(logs.String(), "$2a$") {
				t.Error("a password hash appeared in the log")
			}
		})
	}
}

func TestLegacyRecovery_WritesLegacyRecoveryMarker(t *testing.T) {
	e := newBootEnv(t)
	if err := e.st.SetAdminCreds(store.AdminCreds{Username: "ops", PasswordHash: "not-bcrypt-at-all", Salt: "x"}); err != nil {
		t.Fatalf("SetAdminCreds: %v", err)
	}

	s := e.boot(nil)

	m := e.marker(t)
	if m == nil || m.Origin != bootstrapcred.OriginLegacyRecovery || m.Username != "ops" {
		t.Fatalf("marker = %+v, want legacy_recovery/ops", m)
	}
	if err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080"); err == nil {
		t.Error("legacy recovery is enforced immediately, with no grace")
	}
}

func TestAdminListenerGate_RefusalMatrix(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	tests := []struct {
		name       string
		ip         string
		configured string
		wantServe  bool
	}{
		{"ipv4 loopback", "127.0.0.1", "127.0.0.1:8080", true},
		{"ipv6 loopback", "::1", "[::1]:8080", true},
		{"ipv4 wildcard", "0.0.0.0", "0.0.0.0:8080", false},
		{"ipv6 wildcard", "::", "[::]:8080", false},
		{"empty host", "::", ":8080", false},
		{"LAN address", "192.168.1.20", "192.168.1.20:8080", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.AdminListenerGate(newFakeListener(tt.ip), tt.configured)

			if (err == nil) != tt.wantServe {
				t.Fatalf("gate = %v, wantServe %v", err, tt.wantServe)
			}
			if err != nil {
				pw, _ := bootstrapcred.ReadSafe(e.pwFile())
				for _, want := range []string{"admin dashboard not started", bootstrapcred.EnvAdminBind, bootstrapcred.EnvPasswordFile} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal missing %q: %v", want, err)
					}
				}
				if strings.Contains(err.Error(), pw) {
					t.Error("refusal text leaks the password")
				}
			}
		})
	}
}

func TestAdminListenerGate_RealListeners(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	for bind, wantServe := range map[string]bool{"127.0.0.1:0": true, "0.0.0.0:0": false} {
		ln, err := listenTCP(bind)
		if err != nil {
			t.Skipf("cannot listen on %s: %v", bind, err)
		}
		gateErr := s.AdminListenerGate(ln, bind)
		_ = ln.Close()
		if (gateErr == nil) != wantServe {
			t.Errorf("bind %s: gate = %v, wantServe %v", bind, gateErr, wantServe)
		}
	}
}

func TestAdminListenerGate_EscapeHatch(t *testing.T) {
	tests := []struct {
		value     string
		wantServe bool
	}{
		{"true", true},
		{"TRUE", false},
		{"True", false},
		{"1", false},
		{"yes", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run("value="+tt.value, func(t *testing.T) {
			e := newBootEnv(t)
			env := map[string]string{bootstrapcred.EnvAllowInsecure: tt.value}
			s := e.boot(env)
			logs := captureLog(t)

			err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080")

			if (err == nil) != tt.wantServe {
				t.Fatalf("gate = %v, wantServe %v", err, tt.wantServe)
			}
			entries, _ := e.st.QuerySystemAuditLog(10)
			audited := false
			for _, en := range entries {
				audited = audited || en.Action == "insecure_default_admin_override"
			}
			if audited != tt.wantServe {
				t.Errorf("audit entry present = %v, want %v", audited, tt.wantServe)
			}
			if tt.wantServe && !strings.Contains(logs.String(), bootstrapcred.EnvAllowInsecure) {
				t.Errorf("the override must log a warning naming %s: %s", bootstrapcred.EnvAllowInsecure, logs.String())
			}
		})
	}
}

func TestAdminListenerGate_HatchDoesNotOverrideCredentialFailure(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(map[string]string{bootstrapcred.EnvAllowInsecure: "true", bootstrapcred.EnvPassword: "admin"})

	if err := s.AdminListenerGate(newFakeListener("127.0.0.1"), "127.0.0.1:8080"); err == nil {
		t.Error("the escape hatch must not start a dashboard whose credential setup failed")
	}
}

func TestAdminListenerGate_DemoExemptOnlyByExplicitFlag(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		setDemo   bool
		fresh     bool
		wantServe bool
	}{
		{"no flag on a wildcard bind", nil, false, false, false},
		{"explicit demo env on a non-fresh install", map[string]string{bootstrapcred.EnvDemoMode: "true"}, false, false, true},
		{"demo env must be exactly true", map[string]string{bootstrapcred.EnvDemoMode: "1"}, false, false, false},
		{"explicit demo mode on the server", nil, true, false, true},
		{"demo env never exempts a fresh install", map[string]string{bootstrapcred.EnvDemoMode: "true"}, false, true, false},
		{"demo mode on the server never exempts a fresh install", nil, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			if !tt.fresh {
				e.addAdmin(t, "admin", defaultAdminPassword, true, maxSkipPasswordChanges)
			}
			s := e.boot(tt.env)
			s.SetVersion("v0.26.0") // past the grace release, so grace cannot mask the result
			s.SetDemoMode(tt.setDemo)

			err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080")

			if (err == nil) != tt.wantServe {
				t.Errorf("gate = %v, wantServe %v", err, tt.wantServe)
			}
		})
	}
}

func TestAdminListenerGate_DemoExemptionIsAudited(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", defaultAdminPassword, true, maxSkipPasswordChanges)
	s := e.boot(map[string]string{bootstrapcred.EnvDemoMode: "true"})
	s.SetVersion("v0.26.0")

	if err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080"); err != nil {
		t.Fatalf("gate = %v", err)
	}

	entries, err := e.st.QuerySystemAuditLog(10)
	if err != nil {
		t.Fatalf("QuerySystemAuditLog: %v", err)
	}
	found := false
	for _, en := range entries {
		found = found || en.Action == "insecure_default_admin_override"
	}
	if !found {
		t.Errorf("demo exemption must write a system audit entry, got %+v", entries)
	}
}

func TestAdminListenerGate_UpgradeGraceByOriginAndRelease(t *testing.T) {
	tests := []struct {
		name       string
		mustChange bool
		skips      int
		version    string
		wantServe  bool
	}{
		{"upgraded within the grace release", false, 0, "v0.25.0", true},
		{"upgraded on a development build has no grace", false, 0, "dev", false},
		{"upgraded after the grace release", false, 0, "v0.26.0", false},
		{"legacy recovery has no grace", true, maxSkipPasswordChanges, "v0.25.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			e.addAdmin(t, "admin", defaultAdminPassword, tt.mustChange, tt.skips)
			s := e.boot(nil)
			s.SetVersion(tt.version)

			err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080")

			if (err == nil) != tt.wantServe {
				t.Errorf("gate = %v, wantServe %v", err, tt.wantServe)
			}
		})
	}
}

func TestHealth_PendingFlagIsLoopbackOnly(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	tests := []struct {
		name        string
		remoteAddr  string
		proxyHeader string // a request header a reverse proxy would add, "" for none
		wantField   bool
	}{
		{"loopback v4 client is told", "127.0.0.1:5555", "", true},
		{"loopback v6 client is told", "[::1]:5555", "", true},
		{"remote client sees no field", "192.0.2.10:5555", "", false},
		{"remote client claiming loopback by header sees no field", "192.0.2.10:5555", "X-Forwarded-For", false},
		{"loopback peer relayed by a proxy (X-Forwarded-For) sees no field", "127.0.0.1:5555", "X-Forwarded-For", false},
		{"loopback peer relayed by a proxy (Forwarded) sees no field", "127.0.0.1:5555", "Forwarded", false},
		{"loopback peer relayed by a proxy (X-Real-IP) sees no field", "127.0.0.1:5555", "X-Real-IP", false},
		{"loopback peer relayed by a proxy (CF-Connecting-IP) sees no field", "127.0.0.1:5555", "CF-Connecting-IP", false},
		{"loopback peer relayed by a proxy (True-Client-IP) sees no field", "127.0.0.1:5555", "True-Client-IP", false},
		{"loopback peer relayed by a proxy (X-Client-IP) sees no field", "127.0.0.1:5555", "X-Client-IP", false},
		{"loopback peer relayed by a proxy (Via) sees no field", "127.0.0.1:5555", "Via", false},
		{"loopback peer relayed by a proxy (X-Forwarded-Host) sees no field", "127.0.0.1:5555", "X-Forwarded-Host", false},
		{"loopback peer relayed by a proxy (Forwarded-Host) sees no field", "127.0.0.1:5555", "Forwarded-Host", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.proxyHeader != "" {
				req.Header.Set(tt.proxyHeader, "127.0.0.1")
			}
			rec := httptest.NewRecorder()

			s.Handler().ServeHTTP(rec, req)

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode health: %v", err)
			}
			pending, present := body["bootstrap_password_pending"]
			if present != tt.wantField || (tt.wantField && pending != true) {
				t.Errorf("bootstrap_password_pending = %v (present %v), want present %v and true", pending, present, tt.wantField)
			}
			if _, srcPresent := body["bootstrap_password_source"]; srcPresent != tt.wantField {
				t.Errorf("bootstrap_password_source present = %v, want %v", srcPresent, tt.wantField)
			}
			if strings.Contains(rec.Body.String(), pw) {
				t.Error("health response leaks the password")
			}
		})
	}
}

const (
	testStoredBind   = "0.0.0.0:8080"
	testOverrideBind = "127.0.0.1:8080"
)

func getSettingsBody(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleSettings(rec, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET body: %v", err)
	}
	return body
}

func adminBindOf(t *testing.T, body map[string]any) string {
	t.Helper()
	admin, _ := body["admin"].(map[string]any)
	got, _ := admin["bind_address"].(string)
	return got
}

func putSettingsBody(t *testing.T, s *Server, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetSettings_BindOverrideShowsStoredValueAndFlag(t *testing.T) {
	s := newRealStoreTestServer(t)
	s.cfg.Admin.BindAddress = testStoredBind
	s.SetAdminBindOverride(testOverrideBind, testStoredBind)

	body := getSettingsBody(t, s)

	if got := adminBindOf(t, body); got != testStoredBind {
		t.Errorf("admin.bind_address = %q, want the stored %q", got, testStoredBind)
	}
	if body["admin_bind_override"] != true || body["admin_bind_override_address"] != testOverrideBind {
		t.Errorf("override status = %v / %v, want true / %q", body["admin_bind_override"], body["admin_bind_override_address"], testOverrideBind)
	}
}

func TestGetSettings_NoOverrideReportsNoFlag(t *testing.T) {
	s := newRealStoreTestServer(t)
	s.SetAdminBindOverride("", "")

	body := getSettingsBody(t, s)

	if body["admin_bind_override"] != false {
		t.Errorf("admin_bind_override = %v, want false", body["admin_bind_override"])
	}
	if _, present := body["admin_bind_override_address"]; present {
		t.Errorf("admin_bind_override_address present without an override")
	}
}

func TestUpdateSettings_BindOverrideNeverPersistsTheEnvValue(t *testing.T) {
	s := newRealStoreTestServer(t)
	s.cfg.Admin.BindAddress = testStoredBind
	s.SetAdminBindOverride(testOverrideBind, testStoredBind)

	// A client that round-trips what it read (including the read-only status
	// fields) must leave the stored bind exactly as it was.
	putSettingsBody(t, s, getSettingsBody(t, s))

	if got, err := s.st.GetSetting("admin_bind_address"); err != nil || got != testStoredBind {
		t.Fatalf("stored admin_bind_address = %q (err %v), want %q", got, err, testStoredBind)
	}
	if got := adminBindOf(t, getSettingsBody(t, s)); got != testStoredBind {
		t.Errorf("GET after PUT shows %q, want the stored %q", got, testStoredBind)
	}
}

func TestUpdateSettings_BindOverrideActivePersistsExactlyWhatTheClientSends(t *testing.T) {
	s := newRealStoreTestServer(t)
	s.cfg.Admin.BindAddress = testStoredBind
	s.SetAdminBindOverride(testOverrideBind, testStoredBind)

	const chosen = "127.0.0.1:9999"
	putSettingsBody(t, s, config.Config{Admin: config.AdminConfig{BindAddress: chosen}})

	if got, err := s.st.GetSetting("admin_bind_address"); err != nil || got != chosen {
		t.Fatalf("stored admin_bind_address = %q (err %v), want %q", got, err, chosen)
	}
	body := getSettingsBody(t, s)
	if got := adminBindOf(t, body); got != chosen {
		t.Errorf("GET after PUT shows %q, want %q", got, chosen)
	}
	if body["admin_bind_override_address"] != testOverrideBind {
		t.Errorf("override address changed to %v", body["admin_bind_override_address"])
	}
}

func TestUpdateSettings_NoOverridePersistsTheBind(t *testing.T) {
	s := newRealStoreTestServer(t)
	s.SetAdminBindOverride("", "")

	putSettingsBody(t, s, config.Config{Admin: config.AdminConfig{BindAddress: "127.0.0.1:9999"}})

	if got, err := s.st.GetSetting("admin_bind_address"); err != nil || got != "127.0.0.1:9999" {
		t.Fatalf("stored admin_bind_address = %q (err %v)", got, err)
	}
}

func TestEnsureAdminUser_NopStoreDoesNothing(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)

	s := NewServerWithBootstrap(r, nil, config.Config{}, nil, BootstrapOptions{DataDir: t.TempDir(), Getenv: envFrom(nil)})

	if s.boot.err != nil || s.BootstrapPasswordPending() {
		t.Errorf("a store-less server must not attempt a first-boot credential: err=%v", s.boot.err)
	}
}

func postJSON(h http.Handler, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}
