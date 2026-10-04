package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// failingStore wraps a real store and fails the selected calls.
type failingStore struct {
	store.Store
	failCount   bool
	failSetting bool
	failList    bool
}

var errInjected = errors.New("injected store failure")

func (f *failingStore) CountAdminUsers() (int, error) {
	if f.failCount {
		return 0, errInjected
	}
	return f.Store.CountAdminUsers()
}

func (f *failingStore) GetSetting(key string) (string, error) {
	if f.failSetting {
		return "", errInjected
	}
	return f.Store.GetSetting(key)
}

func (f *failingStore) ListUsers() ([]store.User, error) {
	if f.failList {
		return nil, errInjected
	}
	return f.Store.ListUsers()
}

func (e bootEnv) bootOver(st store.Store) *Server {
	return NewServerWithBootstrap(e.r, nil, config.Config{}, st, BootstrapOptions{DataDir: e.dir, Getenv: envFrom(nil)})
}

func TestBoot_StoreErrorsFailClosed(t *testing.T) {
	seedChanged := func(t *testing.T, e bootEnv) { e.addAdmin(t, "admin", "Already-Changed-Passw0rd", false, 0) }
	tests := []struct {
		name string
		fail func(f *failingStore)
		seed func(t *testing.T, e bootEnv)
	}{
		{"count admins fails on a fresh install", func(f *failingStore) { f.failCount = true }, nil},
		{"marker read fails on an existing install", func(f *failingStore) { f.failSetting = true }, seedChanged},
		{"user listing fails on an existing install", func(f *failingStore) { f.failList = true }, seedChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			if tt.seed != nil {
				tt.seed(t, e)
			}
			f := &failingStore{Store: e.st}
			tt.fail(f)

			s := e.bootOver(f)

			if s.boot.err == nil {
				t.Fatal("a store error in the bootstrap decision path must fail closed")
			}
			if gate := s.AdminListenerGate(newFakeListener("127.0.0.1"), "127.0.0.1:8080"); gate == nil {
				t.Error("the admin listener must not start after a bootstrap failure, even on loopback")
			}
		})
	}
}

func TestBoot_MarkerReadErrorNeverDeletesPasswordFile(t *testing.T) {
	e := newBootEnv(t)
	e.boot(nil)
	if ok, _ := bootstrapcred.Exists(e.pwFile()); !ok {
		t.Fatal("setup: expected a generated password file")
	}

	s := e.bootOver(&failingStore{Store: e.st, failSetting: true})

	if s.boot.err == nil {
		t.Error("an unreadable marker must fail the bootstrap closed")
	}
	if ok, _ := bootstrapcred.Exists(e.pwFile()); !ok {
		t.Error("the password file was deleted although the marker could not be read")
	}
}

func TestBoot_PendingFreshCredentialWithLostFileIsRegenerated(t *testing.T) {
	tests := []struct {
		name  string
		prime func(t *testing.T, path string)
	}{
		{"file missing", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"file empty", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBootEnv(t)
			e.boot(nil)
			oldPW, _ := bootstrapcred.ReadSafe(e.pwFile())
			tt.prime(t, e.pwFile())

			s := e.boot(nil)

			if s.boot.err != nil {
				t.Fatalf("boot failed: %v", s.boot.err)
			}
			newPW, err := bootstrapcred.ReadSafe(e.pwFile())
			if err != nil || newPW == "" || newPW == oldPW {
				t.Fatalf("expected a new generated password in the file, got %q (err %v)", newPW, err)
			}
			u, _ := e.st.GetUserByUsername("admin")
			if !verifyPassword(u.PasswordHash, newPW) || verifyPassword(u.PasswordHash, oldPW) {
				t.Error("the stored hash must match the regenerated password only")
			}
			if !u.MustChangePassword || e.marker(t) == nil {
				t.Error("the forced change and the marker must survive regeneration")
			}
		})
	}
}

func TestBoot_LostFileWithWorkingEnvSecretIsNotRegenerated(t *testing.T) {
	e := newBootEnv(t)
	env := map[string]string{bootstrapcred.EnvPassword: "Supplied-Via-Env-1"}
	e.boot(env)

	s := e.boot(env)

	if s.boot.err != nil {
		t.Fatalf("boot failed: %v", s.boot.err)
	}
	if ok, _ := bootstrapcred.Exists(e.pwFile()); ok {
		t.Error("a password file was written although the supplied secret still works")
	}
	u, _ := e.st.GetUserByUsername("admin")
	if !verifyPassword(u.PasswordHash, "Supplied-Via-Env-1") {
		t.Error("the supplied password must keep working")
	}
}

func TestBoot_SoftDeletedAdminDoesNotWedgeFreshBoot(t *testing.T) {
	e := newBootEnv(t)
	e.addAdmin(t, "admin", "Whatever-Old-Passw0rd", false, 0)
	old, _ := e.st.GetUserByUsername("admin")
	if err := e.st.SoftDeleteUser(old.ID, "test"); err != nil {
		t.Fatal(err)
	}

	s := e.boot(nil)

	if s.boot.err != nil {
		t.Fatalf("boot wedged on a soft-deleted admin: %v", s.boot.err)
	}
	n, err := e.st.CountAdminUsers()
	if err != nil || n != 1 {
		t.Fatalf("active administrators = %d (err %v), want exactly one fresh admin", n, err)
	}
	pw, err := bootstrapcred.ReadSafe(e.pwFile())
	if err != nil {
		t.Fatalf("no generated password: %v", err)
	}
	m := e.marker(t)
	if m == nil || m.Username == "admin" {
		t.Fatalf("marker = %+v, want it on the new, differently named admin", m)
	}
	u, err := e.st.GetUserByUsername(m.Username)
	if err != nil || u.Status != "active" || !verifyPassword(u.PasswordHash, pw) {
		t.Errorf("fresh admin %q not usable with the generated password: %v", m.Username, err)
	}
	if again := e.boot(nil); again.boot.err != nil {
		t.Errorf("second boot failed: %v", again.boot.err)
	}
}

func TestLoginWithBootstrapPassword_ForcesChangeAndRefusesSkip(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	h := s.Handler()

	login := postJSON(h, "/admin/login", map[string]string{"username": "admin", "password": pw}, nil)
	if login.Code != http.StatusOK || !strings.Contains(login.Body.String(), "must_change_password") {
		t.Fatalf("login = %d: %s", login.Code, login.Body.String())
	}
	skip := postJSON(h, "/admin/skip-password-change", map[string]string{}, sessionCookieOf(login))
	if skip.Code != http.StatusForbidden {
		t.Errorf("skip = %d, want 403 for a bootstrap credential", skip.Code)
	}
	if bad := postJSON(h, "/admin/login", map[string]string{"username": "admin", "password": "admin"}, nil); bad.Code == http.StatusOK {
		t.Error("the public default password must not log in on a fresh install")
	}
}

func TestChangePassword_RejectsWeakPasswords(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)
	pw, _ := bootstrapcred.ReadSafe(e.pwFile())
	h := s.Handler()
	cookie := sessionCookieOf(postJSON(h, "/admin/login", map[string]string{"username": "admin", "password": pw}, nil))

	for _, weak := range []string{"admin", "short", strings.Repeat("a", bootstrapcred.MinPasswordLength-1)} {
		rec := postJSON(h, "/admin/change-password", map[string]string{"new_password": weak}, cookie)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "12") {
			t.Errorf("password %q: status %d body %s, want 400 stating the minimum", weak, rec.Code, rec.Body.String())
		}
	}
	if e.marker(t) == nil {
		t.Error("a rejected change must leave the marker in place")
	}
}

func TestGateRefusesNonLoopbackWhilePending(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(nil)

	if err := s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080"); err == nil {
		t.Error("a non-loopback admin listener must be refused while the password is pending")
	}
	if err := s.AdminListenerGate(newFakeListener("127.0.0.1"), "127.0.0.1:8080"); err != nil {
		t.Errorf("loopback must be allowed while pending: %v", err)
	}
}

// TestBootstrapState_ConcurrentTransitionsAreRaceFree exercises the marker, the
// cached flag, the gate and the health handler together under the race detector.
func TestBootstrapState_ConcurrentTransitionsAreRaceFree(t *testing.T) {
	e := newBootEnv(t)
	s := e.boot(map[string]string{bootstrapcred.EnvPassword: "Supplied-Via-Env-1"})
	e.addAdmin(t, "ops", defaultAdminPassword, true, maxSkipPasswordChanges)
	h := s.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); s.onPasswordChanged("admin") }()
		go func() { defer wg.Done(); s.onUserAccessChanged() }()
		go func() {
			defer wg.Done()
			_ = s.AdminListenerGate(newFakeListener("0.0.0.0"), "0.0.0.0:8080")
			_ = s.BootstrapPasswordPending()
		}()
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			req.RemoteAddr = "127.0.0.1:1"
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
}

func TestBoot_SuppliedPasswordIsRemovedFromProcessEnvironment(t *testing.T) {
	const secret = "Supplied-Via-Env-1"
	t.Setenv(bootstrapcred.EnvPassword, secret)
	e := newBootEnv(t)

	s := NewServerWithBootstrap(e.r, nil, config.Config{}, e.st, BootstrapOptions{DataDir: e.dir})

	if s.boot.err != nil {
		t.Fatalf("boot failed: %v", s.boot.err)
	}
	if got := os.Getenv(bootstrapcred.EnvPassword); got != "" {
		t.Errorf("%s still in the process environment after boot", bootstrapcred.EnvPassword)
	}
	u, _ := e.st.GetUserByUsername("admin")
	if !verifyPassword(u.PasswordHash, secret) {
		t.Error("the supplied password must have been applied before it was removed")
	}
}
