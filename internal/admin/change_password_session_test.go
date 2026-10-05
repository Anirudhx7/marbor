package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

var changePasswordRoutes = []struct{ name, path string }{
	{"admin", "/admin/change-password"},
	{"admin_v1", "/admin/v1/change-password"},
	{"session_only", "/change-password"},
}

func mustUser(t *testing.T, s *Server, name string) store.User {
	t.Helper()
	u, err := s.st.GetUserByUsername(name)
	if err != nil {
		t.Fatalf("GetUserByUsername(%s): %v", name, err)
	}
	return u
}

// skippedSessionCookie logs in a temp-password account that still has the
// change pending, skips the forced change, and returns the normal session
// minted by the skip.
func skippedSessionCookie(t *testing.T, s *Server, user, pass string) *http.Cookie {
	t.Helper()
	cookie := sessionCookieFrom(t, loginAs(t, s, user, pass))
	rec := doWithCookie(s, http.MethodPost, "/admin/skip-password-change", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("skip: status = %d body = %s", rec.Code, rec.Body.String())
	}
	return sessionCookieFrom(t, rec)
}

// After "Skip for now" the stored change-pending flag stays set but the session
// is a normal one, so changing the password must still prove the current one.
func TestChangePasswordAfterSkipRequiresCurrentPassword(t *testing.T) {
	for _, rt := range changePasswordRoutes {
		path := rt.path
		t.Run(rt.name, func(t *testing.T) {
			s := newTempPasswordServer(t, true, 0)
			cookie := skippedSessionCookie(t, s, "ops", "Temp-Pass-9")
			before := mustUser(t, s, "ops")
			if !before.MustChangePassword {
				t.Fatal("precondition: stored must-change flag must still be set after the skip")
			}

			rec := doWithCookie(s, http.MethodPost, path, `{"new_password":"Another-Pass-2"}`, cookie)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current_password required") {
				t.Fatalf("no current password: status = %d body = %s, want 400 current_password required", rec.Code, rec.Body.String())
			}
			if after := mustUser(t, s, "ops"); after.PasswordHash != before.PasswordHash {
				t.Error("a rejected change must leave the password hash unchanged")
			}

			rec = doWithCookie(s, http.MethodPost, path, `{"current_password":"Wrong-Pass-1","new_password":"Another-Pass-2"}`, cookie)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "wrong current password") {
				t.Fatalf("wrong current password: status = %d body = %s, want 400 wrong current password", rec.Code, rec.Body.String())
			}

			rec = doWithCookie(s, http.MethodPost, path, `{"current_password":"Temp-Pass-9","new_password":"Another-Pass-2"}`, cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("correct current password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
			}
			if login := loginAs(t, s, "ops", "Another-Pass-2"); login.Code != http.StatusOK {
				t.Errorf("login with the new password: status = %d, want 200", login.Code)
			}
		})
	}
}

// A session that claims a forced change while the stored flag is already clear
// gets no waiver either. This guards the stored-flag half of the waiver
// condition; it is a safety net, not a test that fails without that half.
func TestStaleForcedSessionNeedsCurrentPassword(t *testing.T) {
	s := newTempPasswordServer(t, false, 0)
	u := mustUser(t, s, "ops")
	if err := s.st.CreateUserSession(store.UserSession{
		Token: "stale-forced-session", UserID: u.ID, Role: "admin", Username: "ops",
		MustChangePassword: true, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: "stale-forced-session"}

	rec := doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current_password required") {
		t.Fatalf("no current password: status = %d body = %s, want 400 current_password required", rec.Code, rec.Body.String())
	}
	before := mustUser(t, s, "ops")
	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"current_password":"Wrong-Pass-1","new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong current password: status = %d body = %s, want 400", rec.Code, rec.Body.String())
	}
	if after := mustUser(t, s, "ops"); after.PasswordHash != before.PasswordHash {
		t.Error("a rejected change must leave the password hash unchanged")
	}
	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"current_password":"Temp-Pass-9","new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("with current password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

// newViewerServer returns a server with a non-admin account whose change is pending.
func newViewerServer(t *testing.T) *Server {
	t.Helper()
	s := newTempPasswordServer(t, false, 0)
	hash, err := hashPassword("User-Pass-5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.CreateUser(store.User{
		Username: "viewer", Role: "user", Status: "active", PasswordHash: hash,
		MustChangePassword: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func viewerLogin(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"viewer","password":"User-Pass-5"}`))
	login.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, login)
	return sessionCookieFrom(t, rec)
}

// A non-admin account reaches change-password through the session-only route
// and gets the same rule.
func TestChangePasswordAfterSkipRequiresCurrentPasswordNonAdmin(t *testing.T) {
	s := newViewerServer(t)
	cookie := viewerLogin(t, s)
	rec := doWithCookie(s, http.MethodPost, "/skip-password-change", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("skip: status = %d body = %s", rec.Code, rec.Body.String())
	}
	cookie = sessionCookieFrom(t, rec)
	if !mustUser(t, s, "viewer").MustChangePassword {
		t.Fatal("precondition: stored must-change flag must still be set after the skip")
	}

	rec = doWithCookie(s, http.MethodPost, "/change-password", `{"new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current_password required") {
		t.Fatalf("no current password: status = %d body = %s, want 400 current_password required", rec.Code, rec.Body.String())
	}
	rec = doWithCookie(s, http.MethodPost, "/change-password", `{"current_password":"User-Pass-5","new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct current password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

// A forced non-admin session keeps the waiver on the session-only route.
func TestForcedNonAdminSessionWaivesCurrentPassword(t *testing.T) {
	s := newViewerServer(t)
	cookie := viewerLogin(t, s)
	rec := doWithCookie(s, http.MethodPost, "/change-password", `{"new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("forced change without current password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

// Fail closed: a request whose context carries a username but no forced-change
// flag must prove the current password even though the stored flag is set.
func TestChangePasswordMissingContextFlagFailsClosed(t *testing.T) {
	s := newTempPasswordServer(t, true, 0)
	if !mustUser(t, s, "ops").MustChangePassword {
		t.Fatal("precondition: stored must-change flag must be set")
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/change-password", strings.NewReader(`{"new_password":"Another-Pass-2"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUsername, "ops"))
	rec := httptest.NewRecorder()
	s.handleChangePassword(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current_password required") {
		t.Fatalf("status = %d body = %s, want 400 current_password required", rec.Code, rec.Body.String())
	}
}

// The forced first-login change keeps its waiver on every route.
func TestForcedSessionStillWaivesCurrentPassword(t *testing.T) {
	for _, rt := range changePasswordRoutes {
		path := rt.path
		t.Run(rt.name, func(t *testing.T) {
			s := newTempPasswordServer(t, true, 0)
			cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))
			rec := doWithCookie(s, http.MethodPost, path, `{"new_password":"Another-Pass-2"}`, cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("forced change without current password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}

// A request with no session username (legacy single-credential path) is not
// affected by the session flag. The admin routes only accept session tokens, so
// this path is reachable only by calling the handler directly.
func TestLegacyEmptyUsernameChangePasswordUnchanged(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy-route.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := hashPassword("Legacy-Pass-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAdminCreds(store.AdminCreds{Username: "legacy", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleChangePassword(rec, httptest.NewRequest(http.MethodPost, "/admin/change-password", strings.NewReader(body)))
		return rec
	}
	if rec := call(`{"current_password":"Wrong-Pass-1","new_password":"Legacy-Pass-2"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong current: status = %d body = %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := call(`{"current_password":"Legacy-Pass-1","new_password":"Legacy-Pass-2"}`); rec.Code != http.StatusOK {
		t.Errorf("correct current: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}
