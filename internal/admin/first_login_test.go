package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("no session cookie in response")
	return nil
}

func doWithCookie(s *Server, method, path, body string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:51000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeLogin(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return resp
}

// A fresh install creates admin/admin with the change pending and the skip cap
// already reached, in one record.
func TestFreshInstallAdminIsUnskippable(t *testing.T) {
	s := newRealStoreTestServer(t)
	u, err := s.st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("default admin missing: %v", err)
	}
	if !u.MustChangePassword {
		t.Error("default admin must have MustChangePassword = true")
	}
	if u.SkipPasswordCount != maxSkipPasswordChanges {
		t.Errorf("SkipPasswordCount = %d, want %d", u.SkipPasswordCount, maxSkipPasswordChanges)
	}
}

// The whole first-login journey through the real mux.
func TestFirstLoginMustChangePasswordWithoutSkip(t *testing.T) {
	buf := captureLog(t)
	s := newRealStoreTestServer(t)

	resp := decodeLogin(t, loginAs(t, s, "admin", "admin"))
	if resp["must_change_password"] != true {
		t.Errorf("must_change_password = %v, want true", resp["must_change_password"])
	}
	if resp["can_skip_password_change"] != false {
		t.Errorf("can_skip_password_change = %v, want false", resp["can_skip_password_change"])
	}
	cookie := sessionCookieFrom(t, loginAs(t, s, "admin", "admin"))

	rec := doWithCookie(s, http.MethodPost, "/admin/skip-password-change", "", cookie)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "skip_limit_reached") {
		t.Errorf("skip: status = %d body = %s, want 403 skip_limit_reached", rec.Code, rec.Body.String())
	}

	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/admin/keys"},
		{http.MethodGet, "/admin/settings"},
		{http.MethodGet, "/admin/nodes"},
		{http.MethodPost, "/admin/backup"},
	} {
		rec := doWithCookie(s, ep.method, ep.path, "", cookie)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "password_change_required") {
			t.Errorf("%s %s before the change: status = %d body = %s, want 403 password_change_required", ep.method, ep.path, rec.Code, rec.Body.String())
		}
	}

	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"admin"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("change to the default password: status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if u, _ := s.st.GetUserByUsername("admin"); !u.MustChangePassword {
		t.Error("a rejected change must leave the change pending")
	}

	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"Fresh-Pass-1"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("change to a good password: status = %d, body: %s", rec.Code, rec.Body.String())
	}
	newCookie := sessionCookieFrom(t, rec)

	if rec := loginAs(t, s, "admin", "admin"); rec.Code != http.StatusUnauthorized {
		t.Errorf("old admin/admin login after the change: status = %d, want 401", rec.Code)
	}
	resp = decodeLogin(t, loginAs(t, s, "admin", "Fresh-Pass-1"))
	if resp["must_change_password"] != false {
		t.Errorf("must_change_password after the change = %v, want false", resp["must_change_password"])
	}
	if rec := doWithCookie(s, http.MethodGet, "/admin/keys", "", newCookie); rec.Code != http.StatusOK {
		t.Errorf("session after the change is still gated: status = %d body = %s", rec.Code, rec.Body.String())
	}

	logs := buf.String()
	if !strings.Contains(logs, `admin: initial password for "admin" was changed from 203.0.113.7`) {
		t.Errorf("first-change log line missing: %s", logs)
	}
	if strings.Contains(logs, "Fresh-Pass-1") {
		t.Errorf("log leaked the new password: %s", logs)
	}
}

func newTempPasswordServer(t *testing.T, mustChange bool, skip int) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "temp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := hashPassword("Temp-Pass-9")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(store.User{
		Username: "ops", Role: "admin", Status: "active", PasswordHash: hash,
		MustChangePassword: mustChange, SkipPasswordCount: skip, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{}, st)
}

// An account that is not on the default password (an admin-issued temporary
// password) keeps the skip option, and a forced change may not reuse the
// current password.
func TestTempPasswordAccountCanStillSkip(t *testing.T) {
	buf := captureLog(t)
	s := newTempPasswordServer(t, true, 0)

	rec := loginAs(t, s, "ops", "Temp-Pass-9")
	resp := decodeLogin(t, rec)
	if resp["can_skip_password_change"] != true {
		t.Errorf("can_skip_password_change = %v, want true", resp["can_skip_password_change"])
	}
	cookie := sessionCookieFrom(t, rec)

	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"Temp-Pass-9"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("change to the current password: status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}

	if rec := doWithCookie(s, http.MethodPost, "/admin/skip-password-change", "", cookie); rec.Code != http.StatusOK {
		t.Errorf("skip for a non-default account: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}

	rec = loginAs(t, s, "ops", "Temp-Pass-9")
	cookie = sessionCookieFrom(t, rec)
	if rec := doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"Another-Pass-2"}`, cookie); rec.Code != http.StatusOK {
		t.Fatalf("change: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(buf.String(), "initial password") {
		t.Errorf("the first-change line is only for the default credential: %s", buf.String())
	}
}

// Starting an existing install raises a default-password admin with the change
// pending to the cap, and leaves every other account alone.
func TestStartupRaisesPendingDefaultAdminToSkipCap(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	defHash, _ := hashPassword(defaultAdminPassword)
	otherHash, _ := hashPassword("Some-Pass-3")
	for _, u := range []store.User{
		{Username: "pending", PasswordHash: defHash, MustChangePassword: true},
		{Username: "automation", PasswordHash: defHash, MustChangePassword: false},
		{Username: "changed", PasswordHash: otherHash, MustChangePassword: true},
	} {
		u.Role, u.Status, u.CreatedAt = "admin", "active", time.Now()
		if _, err := st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	NewServer(r, nil, config.Config{}, st)

	want := map[string]struct {
		skip   int
		mustCh bool
	}{
		"pending":    {maxSkipPasswordChanges, true},
		"automation": {0, false},
		"changed":    {0, true},
	}
	for name, w := range want {
		u, err := st.GetUserByUsername(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if u.SkipPasswordCount != w.skip || u.MustChangePassword != w.mustCh {
			t.Errorf("%s: skip=%d mustChange=%v, want skip=%d mustChange=%v", name, u.SkipPasswordCount, u.MustChangePassword, w.skip, w.mustCh)
		}
	}
}

func TestChangePasswordEnforcesMinimumLength(t *testing.T) {
	s := newRealStoreTestServer(t)
	cookie := sessionCookieFrom(t, loginAs(t, s, "admin", "admin"))

	rec := doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"short77"}`, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "password must be at least 8 characters") {
		t.Errorf("7-character password: status = %d body = %s, want 400 with the length message", rec.Code, rec.Body.String())
	}
	if u, _ := s.st.GetUserByUsername("admin"); !u.MustChangePassword {
		t.Error("a rejected change must leave the change pending")
	}
	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"exactly8"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Errorf("8-character password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

// Two forced changes racing on the same account: exactly one is accepted and
// the stored password is the winner's.
func TestConcurrentForcedPasswordChangeHasOneWinner(t *testing.T) {
	s := newTempPasswordServer(t, true, 0)
	const n = 8
	cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))

	type result struct {
		pass string
		code int
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		pass := "Racing-Pass-" + string(rune('A'+i))
		go func() {
			<-start
			rec := doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"`+pass+`"}`, cookie)
			results <- result{pass, rec.Code}
		}()
	}
	close(start)
	winners := []string{}
	for i := 0; i < n; i++ {
		if r := <-results; r.code == http.StatusOK {
			winners = append(winners, r.pass)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("accepted changes = %v, want exactly one", winners)
	}
	u, err := s.st.GetUserByUsername("ops")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(u.PasswordHash, winners[0]) {
		t.Errorf("stored password does not match the accepted change %q", winners[0])
	}
	if u.MustChangePassword {
		t.Error("change flag still set after an accepted change")
	}
}

func TestStartupSkipCapEndsExistingSessions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "revoke.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	defHash, _ := hashPassword(defaultAdminPassword)
	id, err := st.CreateUser(store.User{
		Username: "pending", Role: "admin", Status: "active", PasswordHash: defHash,
		MustChangePassword: true, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUserSession(store.UserSession{
		Token: "old-skippable-session", UserID: id, Role: "admin", Username: "pending",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	NewServer(r, nil, config.Config{}, st)

	if _, found, _ := st.GetUserSession("old-skippable-session"); found {
		t.Error("a session from before the skip cap was applied is still valid")
	}
}

func TestSkipWithNoChangePendingIsRejected(t *testing.T) {
	s := newTempPasswordServer(t, false, 0)
	cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))
	rec := doWithCookie(s, http.MethodPost, "/admin/skip-password-change", "", cookie)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_password_change_pending") {
		t.Errorf("skip with nothing pending: status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if u, _ := s.st.GetUserByUsername("ops"); u.SkipPasswordCount != 0 {
		t.Errorf("SkipPasswordCount = %d, want 0", u.SkipPasswordCount)
	}
}

// An admin-issued temporary password starts with a fresh skip allowance.
func TestResetPasswordRestoresSkipAllowance(t *testing.T) {
	s := newTempPasswordServer(t, false, 0)
	hash, _ := hashPassword("Root-Pass-77")
	if _, err := s.st.CreateUser(store.User{
		Username: "capped", Role: "admin", Status: "active", PasswordHash: hash,
		MustChangePassword: true, SkipPasswordCount: maxSkipPasswordChanges, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	target, _ := s.st.GetUserByUsername("capped")
	cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))

	rec := doWithCookie(s, http.MethodPost, "/admin/v1/users/"+strconv.FormatInt(target.ID, 10)+"/reset-password", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status = %d body = %s", rec.Code, rec.Body.String())
	}
	got, _ := s.st.GetUserByUsername("capped")
	if got.SkipPasswordCount != 0 || !got.MustChangePassword {
		t.Errorf("after reset: skip=%d mustChange=%v, want 0/true", got.SkipPasswordCount, got.MustChangePassword)
	}
}

// The legacy single-credential change applies the same new-password rules.
func TestLegacyChangePasswordAppliesNewPasswordRules(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy-rules.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := hashPassword("Legacy-Pass-1")
	if err := st.SetAdminCreds(store.AdminCreds{Username: "legacy", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	for name, newPass := range map[string]string{"default": "admin", "short": "abc1234", "same": "Legacy-Pass-1", "empty": ""} {
		rec := httptest.NewRecorder()
		s.handleChangePasswordLegacy(rec, "Legacy-Pass-1", newPass)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body = %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	s.handleChangePasswordLegacy(rec, "Legacy-Pass-1", "Legacy-Pass-2")
	if rec.Code != http.StatusOK {
		t.Errorf("valid change: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}
