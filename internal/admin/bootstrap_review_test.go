package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// crashStore fails the selected writes so a first boot can be interrupted
// between its steps.
type crashStore struct {
	store.Store
	failCreateUser bool
	failSetKey     string // a settings key whose write fails, "" for none
}

func (c *crashStore) CreateUser(u store.User) (int64, error) {
	if c.failCreateUser {
		return 0, errInjected
	}
	return c.Store.CreateUser(u)
}

func (c *crashStore) SetSetting(key, val string) error {
	if c.failSetKey != "" && key == c.failSetKey {
		return errInjected
	}
	return c.Store.SetSetting(key, val)
}

func (e bootEnv) bootOverEnv(st store.Store, env map[string]string) *Server {
	return NewServerWithBootstrap(e.r, nil, config.Config{}, st, BootstrapOptions{DataDir: e.dir, Getenv: envFrom(env)})
}

const suppliedPW = "Supplied-Via-Env-1"

func TestFirstBoot_UserWriteFailureLeavesNoAdminAndNoMarker(t *testing.T) {
	e := newBootEnv(t)

	s := e.bootOverEnv(&crashStore{Store: e.st, failCreateUser: true}, map[string]string{bootstrapcred.EnvPassword: suppliedPW})

	if s.boot.err == nil {
		t.Fatal("a failed user write must fail the first boot closed")
	}
	if n, _ := e.st.CountAdminUsers(); n != 0 {
		t.Errorf("administrators = %d, want none", n)
	}
	if e.marker(t) != nil {
		t.Error("the marker written ahead of the user must be cleared again when the user write fails")
	}
}

func TestFirstBoot_MarkerWriteFailureCreatesNoAdmin(t *testing.T) {
	e := newBootEnv(t)

	s := e.bootOverEnv(&crashStore{Store: e.st, failSetKey: bootstrapcred.SettingKey}, map[string]string{bootstrapcred.EnvPassword: suppliedPW})

	if s.boot.err == nil {
		t.Fatal("a failed marker write must fail the first boot closed")
	}
	if n, _ := e.st.CountAdminUsers(); n != 0 {
		t.Errorf("administrators = %d: an admin must never exist without its marker", n)
	}
}

// A crash after the marker is written and before the user row exists leaves a
// marker and no administrator. The next start must complete the boot with the
// forced change, the skip cap and a matching marker.
func TestFirstBoot_CrashAfterMarkerBeforeUserCompletesOnRestart(t *testing.T) {
	e := newBootEnv(t)
	stale := bootstrapcred.Marker{Username: "admin", CreatedAt: time.Now().UTC(), Origin: bootstrapcred.OriginFresh, Supplied: true}
	raw, err := stale.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetSetting(bootstrapcred.SettingKey, raw); err != nil {
		t.Fatal(err)
	}

	s := e.boot(map[string]string{bootstrapcred.EnvPassword: suppliedPW})

	if s.boot.err != nil {
		t.Fatalf("boot failed: %v", s.boot.err)
	}
	u, err := e.st.GetUserByUsername("admin")
	if err != nil || !u.MustChangePassword || u.SkipPasswordCount < maxSkipPasswordChanges || !verifyPassword(u.PasswordHash, suppliedPW) {
		t.Fatalf("admin = %+v (err %v), want forced change, skip cap and the supplied password", u, err)
	}
	if m := e.marker(t); m == nil || m.Username != "admin" || !s.BootstrapPasswordPending() {
		t.Errorf("marker = %+v pending=%v", m, s.BootstrapPasswordPending())
	}
}

func TestLegacyRecovery_CrashAfterMarkerBeforeUserStaysEnforced(t *testing.T) {
	e := newBootEnv(t)
	if err := e.st.SetAdminCreds(store.AdminCreds{Username: "ops", PasswordHash: "not-bcrypt-at-all", Salt: "x"}); err != nil {
		t.Fatal(err)
	}
	stale := bootstrapcred.Marker{Username: "ops", CreatedAt: time.Now().UTC(), Origin: bootstrapcred.OriginLegacyRecovery}
	raw, _ := stale.Encode()
	if err := e.st.SetSetting(bootstrapcred.SettingKey, raw); err != nil {
		t.Fatal(err)
	}

	s := e.boot(nil)

	m := e.marker(t)
	if m == nil || m.Origin != bootstrapcred.OriginLegacyRecovery {
		t.Fatalf("marker = %+v, want legacy_recovery", m)
	}
	u, _ := e.st.GetUserByUsername("ops")
	if u.SkipPasswordCount < maxSkipPasswordChanges || !u.MustChangePassword {
		t.Errorf("recovered admin must be forced and unskippable: %+v", u)
	}
	if err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080"); err == nil {
		t.Error("legacy recovery must be enforced at once, never granted grace")
	}
}

// A sole administrator that is forced to change its password but has no marker
// is a reset account, not a first boot: the marker is written ahead of the user
// row, so an unmarked forced-change admin is never an interrupted first boot. It
// must get no marker and must not be locked out.
func TestBoot_SoleForcedChangeAdminWithoutMarkerGetsNoMarker(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", suppliedPW, true, 0)

	s := e.boot(nil)

	if m := e.marker(t); m != nil {
		t.Errorf("marker = %+v, want none for a reset forced-change admin", m)
	}
	if s.BootstrapPasswordPending() {
		t.Error("pending flag must not be set for a non-default, unmarked admin")
	}
	if err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080"); err != nil {
		t.Errorf("the gate must allow serving, got %v", err)
	}
}

func TestBoot_ForcedChangeAdminAmongSeveralGetsNoMarker(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", "Already-Changed-Passw0rd", false, 0)
	e.addAdmin(t, "second", "Another-Strong-Passw0rd", true, 0)

	e.boot(nil)

	if m := e.marker(t); m != nil {
		t.Errorf("marker = %+v, want none", m)
	}
}

// A leftover generated password file next to an upgraded or legacy-recovery
// marker is stale: it is removed, and a file with unsafe permissions must not
// fail the flow closed.
func TestBoot_StaleGeneratedFileBesideUpgradedMarkerIsRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	e := newBootEnv(t)
	e.addAdmin(t, "admin", defaultAdminPassword, false, 0)
	if err := bootstrapcred.WriteNew(e.pwFile(), "stale-generated-secret-value"); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if err := os.Chmod(e.pwFile(), 0o666); err != nil {
		t.Fatal(err)
	}

	s := e.boot(nil)

	if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
		t.Error("the stale generated file must be removed")
	}
	if err := s.boot.err; err != nil {
		t.Errorf("a stale unsafe file must not fail the flow closed: %v", err)
	}
	if !s.BootstrapPasswordPending() {
		t.Error("the default-password admin must still be pending")
	}
}

// Concurrent refreshes and health reads never observe a torn pending/source pair.
func TestBoot_PendingSnapshotIsConsistentUnderRace(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.refreshBootstrapPending()
		}
	}()
	for i := 0; i < 2000; i++ {
		if src := s.BootstrapPasswordSource(); src != "" && src != bootstrapcred.PasswordSourceFile {
			t.Fatalf("source = %q, want %q", src, bootstrapcred.PasswordSourceFile)
		}
	}
	<-done
}

// The operator-supplied credential is never replaced by a generated one, not even
// when the supplying variable was removed after the first boot.
func TestBoot_SuppliedPasswordSurvivesEnvironmentRemoval(t *testing.T) {
	e := newBootEnv(t)
	e.boot(map[string]string{bootstrapcred.EnvPassword: suppliedPW})
	if m := e.marker(t); m == nil || !m.Supplied {
		t.Fatalf("marker = %+v, want supplied", m)
	}

	s := e.boot(nil) // the variable is gone (env removed, secret unmounted)

	if s.boot.err != nil {
		t.Fatalf("boot failed: %v", s.boot.err)
	}
	if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
		t.Error("a generated password file was written over a supplied credential")
	}
	u, _ := e.st.GetUserByUsername("admin")
	if !verifyPassword(u.PasswordHash, suppliedPW) {
		t.Error("the supplied password was overwritten")
	}
	if !s.BootstrapPasswordPending() || s.BootstrapPasswordSource() != bootstrapcred.PasswordSourceSupplied {
		t.Errorf("pending=%v source=%q, want pending/supplied", s.BootstrapPasswordPending(), s.BootstrapPasswordSource())
	}
}

func TestBoot_SuppliedCredentialIgnoresLeftoverGeneratedFile(t *testing.T) {
	e := newBootEnv(t)
	e.boot(map[string]string{bootstrapcred.EnvPassword: suppliedPW})
	if err := bootstrapcred.WriteNew(e.pwFile(), "Stale-Generated-1234"); err != nil {
		t.Fatal(err)
	}

	s := e.boot(nil)

	if s.boot.err != nil {
		t.Fatalf("boot failed: %v", s.boot.err)
	}
	if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
		t.Error("a stale generated file beside a supplied credential must be removed")
	}
}

func TestRegeneratePendingPassword_NeverTouchesSuppliedOrDeletedAdmin(t *testing.T) {
	t.Run("supplied marker", func(t *testing.T) {
		e := newBootEnv(t)
		s := e.boot(map[string]string{bootstrapcred.EnvPassword: suppliedPW})
		before, _ := e.st.GetUserByUsername("admin")

		err := s.regeneratePendingPassword(e.pwFile(), bootstrapcred.Marker{Username: "admin", Origin: bootstrapcred.OriginFresh, Supplied: true})

		after, _ := e.st.GetUserByUsername("admin")
		if err != nil || after.PasswordHash != before.PasswordHash {
			t.Errorf("err=%v; the supplied password hash must not change", err)
		}
	})
	t.Run("soft-deleted admin that still reads as active", func(t *testing.T) {
		e := newBootEnv(t)
		s := e.boot(nil)
		u, _ := e.st.GetUserByUsername("admin")
		if err := e.st.SoftDeleteUser(u.ID, "test"); err != nil {
			t.Fatal(err)
		}
		// Force the worst case the explicit check guards against: the deleted
		// row reads back as an active admin that must change its password.
		deleted, _ := e.st.GetUserByUsername("admin")
		if deleted.DeletedAt == nil {
			t.Fatal("setup: user was not soft-deleted")
		}
		deleted.Status = "active"
		if err := e.st.UpdateUser(deleted); err != nil {
			t.Fatal(err)
		}
		hashBefore := deleted.PasswordHash

		err := s.regeneratePendingPassword(e.pwFile(), bootstrapcred.Marker{Username: "admin", Origin: bootstrapcred.OriginFresh})

		after, _ := e.st.GetUserByUsername("admin")
		if err != nil || after.PasswordHash != hashBefore {
			t.Errorf("err=%v; a deleted administrator must not get a regenerated password", err)
		}
	})
}

func healthBody(t *testing.T, s *Server, remote string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return body
}

func TestHealth_ReportsWhereThePendingPasswordComesFrom(t *testing.T) {
	tests := []struct {
		name string
		boot func(t *testing.T) *Server
		want string
	}{
		{"generated file", func(t *testing.T) *Server { return newBootEnv(t).boot(nil) }, "file"},
		{"supplied by the operator", func(t *testing.T) *Server {
			return newBootEnv(t).boot(map[string]string{bootstrapcred.EnvPassword: suppliedPW})
		}, "supplied"},
		{"upgraded default admin", func(t *testing.T) *Server {
			e := newBootEnv(t)
			e.addAdmin(t, "admin", defaultAdminPassword, false, 0)
			return e.boot(nil)
		}, "default"},
		{"legacy recovery", func(t *testing.T) *Server {
			e := newBootEnv(t)
			if err := e.st.SetAdminCreds(store.AdminCreds{Username: "ops", PasswordHash: "not-bcrypt", Salt: "x"}); err != nil {
				t.Fatal(err)
			}
			return e.boot(nil)
		}, "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.boot(t)

			body := healthBody(t, s, "127.0.0.1:1")

			if body["bootstrap_password_pending"] != true || body["bootstrap_password_source"] != tt.want {
				t.Errorf("health = %v, want pending with source %q", body, tt.want)
			}
		})
	}
}

func TestHealth_NoSourceOnceThePasswordIsChanged(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	cookie := sessionCookieOf(postJSON(s.Handler(), "/admin/login", map[string]string{"username": "admin", "password": pw}, nil))
	if rec := postJSON(s.Handler(), "/admin/change-password", map[string]string{"new_password": "A-Brand-New-Passw0rd"}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("change-password = %d: %s", rec.Code, rec.Body.String())
	}

	body := healthBody(t, s, "127.0.0.1:1")

	if body["bootstrap_password_pending"] != false {
		t.Errorf("pending = %v, want false", body["bootstrap_password_pending"])
	}
	if _, present := body["bootstrap_password_source"]; present {
		t.Error("no source may be reported when nothing is pending")
	}
}

func TestChangePassword_RejectsTheCurrentPasswordAsTheNewOne(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	h := s.Handler()
	cookie := sessionCookieOf(postJSON(h, "/admin/login", map[string]string{"username": "admin", "password": pw}, nil))

	rec := postJSON(h, "/admin/change-password", map[string]string{"new_password": pw}, cookie)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "different") {
		t.Errorf("status %d body %s, want 400 saying the password must differ", rec.Code, rec.Body.String())
	}
	if e.marker(t) == nil {
		t.Error("a rejected change must leave the marker in place")
	}
}

func TestChangePassword_PolicyCountsCharactersNotBytes(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	h := s.Handler()
	cookie := sessionCookieOf(postJSON(h, "/admin/login", map[string]string{"username": "admin", "password": pw}, nil))

	short := strings.Repeat("é", bootstrapcred.MinPasswordLength-1) // 22 bytes, 11 characters
	rec := postJSON(h, "/admin/change-password", map[string]string{"new_password": short}, cookie)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("11 multi-byte characters = %d, want 400", rec.Code)
	}
}

func putSettings(t *testing.T, s *Server, body any) *httptest.ResponseRecorder {
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
	return rec
}

func warningsOf(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Body.Len() == 0 {
		return nil
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode settings response %q: %v", rec.Body.String(), err)
	}
	return out.Warnings
}

func TestUpdateSettings_WarnsWhenANonLoopbackBindWouldLockOutTheOperator(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		bind     string
		version  string
		wantWarn bool
	}{
		{"wildcard bind while a password is pending", nil, "0.0.0.0:8080", "v0.26.0", true},
		{"empty host while a password is pending", nil, ":8080", "v0.26.0", true},
		{"loopback bind is safe", nil, "127.0.0.1:8080", "v0.26.0", false},
		{"v6 loopback bind is safe", nil, "[::1]:8080", "v0.26.0", false},
		{"escape hatch makes it safe", map[string]string{bootstrapcred.EnvAllowInsecure: "true"}, "0.0.0.0:8080", "v0.26.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			env := map[string]string{bootstrapcred.EnvPassword: suppliedPW}
			for k, v := range tt.env {
				env[k] = v
			}
			s := e.boot(env)
			s.SetVersion(tt.version)

			rec := putSettings(t, s, config.Config{Admin: config.AdminConfig{BindAddress: tt.bind}})

			warnings := warningsOf(t, rec)
			if (len(warnings) > 0) != tt.wantWarn {
				t.Fatalf("warnings = %v, want warning %v", warnings, tt.wantWarn)
			}
			if tt.wantWarn && (!strings.Contains(warnings[0], tt.bind) || strings.Contains(warnings[0], suppliedPW)) {
				t.Errorf("warning %q must name the bind and never a password", warnings[0])
			}
		})
	}
}

func TestUpdateSettings_NoWarningOnceThePasswordIsChanged(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", "Already-Changed-Passw0rd", false, 0)
	s := e.boot(nil)

	rec := putSettings(t, s, config.Config{Admin: config.AdminConfig{BindAddress: "0.0.0.0:8080"}})

	if w := warningsOf(t, rec); len(w) != 0 {
		t.Errorf("warnings = %v, want none when no initial password is active", w)
	}
}

func TestBoot_LegacyAdminNameHeldByDeletedUserFailsWithAnActionableError(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "legacyadmin", "Whatever-Old-Passw0rd", false, 0)
	old, _ := e.st.GetUserByUsername("legacyadmin")
	if err := e.st.SoftDeleteUser(old.ID, "test"); err != nil {
		t.Fatal(err)
	}
	hash, err := hashPassword("Legacy-Strong-Passw0rd")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAdminCreds(store.AdminCreds{Username: "legacyadmin", PasswordHash: hash, Salt: "00"}); err != nil {
		t.Fatal(err)
	}

	s := e.boot(nil)

	if s.boot.err == nil || !strings.Contains(s.boot.err.Error(), "already exists") || !strings.Contains(s.boot.err.Error(), "Rename or permanently remove") {
		t.Errorf("err = %v, want an actionable error naming the clashing user", s.boot.err)
	}
}
