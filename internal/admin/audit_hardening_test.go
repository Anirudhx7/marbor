package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// syncBuffer is a goroutine-safe log sink: NewServer starts goroutines that
// may log while the test reads the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// A malformed persisted expires_at must show as expired in the key list, never
// active, matching what auth enforces.
func TestAdmin_KeyListMalformedExpiryNotActive(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{Auth: config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "bad", Key: "sk-bad", RateLimit: 1000, ExpiresAt: "garbage"},
			{Name: "ok", Key: "sk-ok", RateLimit: 1000, ExpiresAt: "2099-01-01"},
			{Name: "none", Key: "sk-none", RateLimit: 1000},
		},
	}}
	a := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, a, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/keys status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	want := map[string]string{"bad": "expired", "ok": "active", "none": "active"}
	got := map[string]string{}
	for _, k := range list {
		got[k.Name] = k.Status
	}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("key %q status = %q, want %q (all: %v)", name, got[name], status, got)
		}
	}
}

func loginAs(t *testing.T, s *Server, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func newLegacyServer(t *testing.T, username, passwordHash string) (*Server, store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetAdminCreds(store.AdminCreds{Username: username, PasswordHash: passwordHash, Salt: "00"}); err != nil {
		t.Fatalf("SetAdminCreds: %v", err)
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{}, st), st // migrates the legacy row into users
}

// A legacy row holding an old iterated SHA-256 hash cannot be verified by the
// bcrypt-only login. The migration must not copy it: it recreates the legacy
// username as an active admin on the documented default password with a forced
// change, warns naming the account (never the hash), and the old password no
// longer works.
func TestLegacySHA256AdminRecoveredWithForcedPasswordChange(t *testing.T) {
	buf := captureLog(t)
	legacyHash := strings.Repeat("0", 64) // shape of a hex sha256; just not bcrypt
	s, st := newLegacyServer(t, "legacyadmin", legacyHash)

	u, err := st.GetUserByUsername("legacyadmin")
	if err != nil {
		t.Fatalf("legacy username not preserved: %v", err)
	}
	if _, err := st.GetUserByUsername("admin"); err == nil {
		t.Error("a separate default 'admin' user was created; the legacy username must be reused")
	}
	if u.Role != "admin" || u.Status != "active" {
		t.Errorf("recovered user role/status = %q/%q, want admin/active", u.Role, u.Status)
	}
	if !u.MustChangePassword {
		t.Error("recovered admin must have MustChangePassword = true")
	}
	if u.PasswordHash == legacyHash || !isBcryptHash(u.PasswordHash) {
		t.Error("the unusable legacy hash was copied into users instead of being replaced")
	}
	if !verifyPassword(u.PasswordHash, defaultAdminPassword) {
		t.Error("recovered admin does not verify against the documented default password")
	}

	logs := buf.String()
	if !strings.Contains(logs, `"legacyadmin"`) || !strings.Contains(logs, "Log in") {
		t.Errorf("startup warning must name the account and tell the admin to change the password: %s", logs)
	}
	if strings.Contains(logs, legacyHash) {
		t.Errorf("startup log leaked the legacy hash: %s", logs)
	}
	if logLeaksDefaultPassword(logs) {
		t.Errorf("startup log leaked the default password: %s", logs)
	}

	if rec := loginAs(t, s, "legacyadmin", "legacy-Passw0rd!"); rec.Code != http.StatusUnauthorized {
		t.Errorf("login with an old legacy password: status = %d, want 401", rec.Code)
	}

	// Default password works, but only into the forced-change flow.
	rec := loginAs(t, s, "legacyadmin", defaultAdminPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with the recovery password: status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if resp["must_change_password"] != true {
		t.Errorf("login response must_change_password = %v, want true", resp["must_change_password"])
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie after login")
	}
	skipReq := httptest.NewRequest(http.MethodPost, "/admin/skip-password-change", nil)
	skipReq.AddCookie(cookie)
	skipRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(skipRec, skipReq)
	if skipRec.Code != http.StatusForbidden || !strings.Contains(skipRec.Body.String(), "skip_limit_reached") {
		t.Errorf("skipping the forced change on a recovered default-password account: status = %d body = %s, want 403 skip_limit_reached", skipRec.Code, skipRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.AddCookie(cookie)
	blocked := httptest.NewRecorder()
	s.Handler().ServeHTTP(blocked, req)
	if blocked.Code != http.StatusForbidden || !strings.Contains(blocked.Body.String(), "password_change_required") {
		t.Errorf("normal admin API with a must-change session: status = %d body = %s, want 403 password_change_required", blocked.Code, blocked.Body.String())
	}

}

// A legacy row that already holds a bcrypt hash migrates unchanged: same
// username, same hash, no forced change, and the existing password still works.
func TestLegacyBcryptAdminMigratesUnchanged(t *testing.T) {
	hash, err := hashPassword("Ops-Secret-1")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	s, st := newLegacyServer(t, "ops", hash)

	u, err := st.GetUserByUsername("ops")
	if err != nil {
		t.Fatalf("legacy username not preserved: %v", err)
	}
	if u.PasswordHash != hash {
		t.Error("a valid bcrypt hash must be migrated as is")
	}
	if u.MustChangePassword {
		t.Error("a valid bcrypt migration must keep MustChangePassword = false")
	}
	if rec := loginAs(t, s, "ops", "Ops-Secret-1"); rec.Code != http.StatusOK {
		t.Errorf("login with the carried-over password: status = %d, want 200", rec.Code)
	}
	if rec := loginAs(t, s, "ops", defaultAdminPassword); rec.Code != http.StatusUnauthorized {
		t.Errorf("default password must not work on a migrated bcrypt account: status = %d, want 401", rec.Code)
	}
}

// The default-credential warning fires on first creation and again on a restart
// while the password is still admin, and stops once it has been changed.
func TestDefaultAdminCredentialWarning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "warn.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)

	buf := captureLog(t)
	NewServer(r, nil, config.Config{}, st)
	for _, want := range []string{`"admin" / "admin"`, "password change is required", "plaintext HTTP", "TLS"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("first boot warning missing %q: %s", want, buf.String())
		}
	}

	buf.Reset()
	NewServer(r, nil, config.Config{}, st) // restart, password unchanged
	if !strings.Contains(buf.String(), `default admin login "admin" / "admin" is still active`) {
		t.Errorf("restart with default credential gave no warning: %s", buf.String())
	}

	newHash, err := hashPassword("a-Much-Better-Passw0rd")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	u, _ := st.GetUserByUsername("admin")
	u.PasswordHash = newHash
	u.MustChangePassword = false
	if err := st.UpdateUser(u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	buf.Reset()
	NewServer(r, nil, config.Config{}, st)
	if strings.Contains(buf.String(), `"admin" / "admin"`) {
		t.Errorf("warning still logged after the password was changed: %s", buf.String())
	}
}

// Whitespace around the legacy username is trimmed; an empty or blank one falls
// back to "admin"; an empty legacy hash is unusable and is recovered too.
func TestLegacyAdminUsernameAndEmptyHashEdgeCases(t *testing.T) {
	for _, tt := range []struct {
		name, legacyUser, wantUser string
	}{
		{"padded username trimmed", "  ops  ", "ops"},
		{"blank username falls back to admin", "   ", "admin"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, st := newLegacyServer(t, tt.legacyUser, "") // empty hash: not bcrypt
			u, err := st.GetUserByUsername(tt.wantUser)
			if err != nil {
				t.Fatalf("user %q not created: %v", tt.wantUser, err)
			}
			if !u.MustChangePassword || !verifyPassword(u.PasswordHash, defaultAdminPassword) {
				t.Errorf("empty legacy hash must be recovered onto the default password with a forced change")
			}
		})
	}
}

// failingCreateStore makes CreateUser fail so the legacy branch's error path is
// exercised.
type failingCreateStore struct{ store.Store }

func (failingCreateStore) CreateUser(store.User) (int64, error) {
	return 0, errors.New("simulated create failure")
}

// failingSkipCapStore lets CreateUser succeed but fails the first UpdateUser
// that follows it, which is the write that stores the skip cap during legacy
// admin recovery. Later updates pass through untouched.
type failingSkipCapStore struct {
	store.Store
	mu          sync.Mutex
	failNextUpd bool
}

func (f *failingSkipCapStore) CreateUser(u store.User) (int64, error) {
	id, err := f.Store.CreateUser(u)
	if err == nil {
		f.mu.Lock()
		f.failNextUpd = true
		f.mu.Unlock()
	}
	return id, err
}

func (f *failingSkipCapStore) UpdateUser(u store.User) error {
	f.mu.Lock()
	fail := f.failNextUpd
	f.failNextUpd = false
	f.mu.Unlock()
	if fail {
		return errors.New("simulated skip-cap write failure")
	}
	return f.Store.UpdateUser(u)
}

// logLeaksDefaultPassword reports whether logs print the default password as
// a standalone value. The default password is the word admin, which also
// appears inside account names and prose, so only the quoted and
// "password: value" forms count as a leak.
func logLeaksDefaultPassword(logs string) bool {
	for _, form := range []string{`"` + defaultAdminPassword + `"`, `'` + defaultAdminPassword + `'`, "password: " + defaultAdminPassword, "password=" + defaultAdminPassword} {
		if strings.Contains(logs, form) {
			return true
		}
	}
	return false
}

// If the skip-cap write fails after the recovered account was created, the
// account is removed rather than left with a skippable forced change. Nothing
// may be left on the default password, no second default admin may appear, and
// neither the default password nor the legacy hash may be logged.
func TestLegacyRecoverySkipCapWriteFailureRemovesAccount(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy-skipcap.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	legacyHash := strings.Repeat("0", 64)
	if err := st.SetAdminCreds(store.AdminCreds{Username: "legacyadmin", PasswordHash: legacyHash, Salt: "00"}); err != nil {
		t.Fatalf("SetAdminCreds: %v", err)
	}
	buf := captureLog(t)
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, &failingSkipCapStore{Store: st})

	if _, err := st.GetUserByUsername("legacyadmin"); err == nil {
		t.Error("recovered user row still exists after the skip-cap write failed")
	}
	if _, err := st.GetUserByUsername("admin"); err == nil {
		t.Error("a default admin user was created after the failed recovery")
	}
	if n, _ := st.CountAdminUsers(); n != 0 {
		t.Errorf("%d admin users exist after the failed recovery, want 0", n)
	}
	for _, user := range []string{"legacyadmin", "admin"} {
		if rec := loginAs(t, s, user, defaultAdminPassword); rec.Code == http.StatusOK {
			t.Errorf("login as %q with the default password succeeded after the failed recovery", user)
		}
	}

	logs := buf.String()
	if !strings.Contains(logs, "could not migrate legacy admin") {
		t.Errorf("failure was not logged: %s", logs)
	}
	if logLeaksDefaultPassword(logs) {
		t.Errorf("log leaked the default password: %s", logs)
	}
	if strings.Contains(logs, legacyHash) {
		t.Errorf("log leaked the legacy hash: %s", logs)
	}
}

// A failed legacy migration must not fall through to creating a second
// admin/admin account on the default password.
func TestLegacyMigrationCreateFailureDoesNotCreateDefaultAdmin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy-fail.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetAdminCreds(store.AdminCreds{Username: "legacyadmin", PasswordHash: strings.Repeat("0", 64), Salt: "00"}); err != nil {
		t.Fatalf("SetAdminCreds: %v", err)
	}
	buf := captureLog(t)
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	NewServer(r, nil, config.Config{}, failingCreateStore{st})

	if n, _ := st.CountAdminUsers(); n != 0 {
		t.Errorf("%d admin users exist after a failed migration; none should be created", n)
	}
	if !strings.Contains(buf.String(), "could not migrate legacy admin") {
		t.Errorf("failure was not logged: %s", buf.String())
	}
}

// After a recovery, a restart still warns, naming the preserved (non-"admin")
// username, until the password is changed.
func TestRecoveredAdminKeepsWarningOnRestart(t *testing.T) {
	buf := captureLog(t)
	s, st := newLegacyServer(t, "legacyadmin", strings.Repeat("0", 64))
	_ = s
	buf.Reset()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	NewServer(r, nil, config.Config{}, st) // restart
	if !strings.Contains(buf.String(), `"legacyadmin" / "admin" is still active`) {
		t.Errorf("restart did not warn about the recovered default-password account: %s", buf.String())
	}
}
