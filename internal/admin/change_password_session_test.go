package admin

import (
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
	for _, path := range []string{"/admin/change-password", "/admin/v1/change-password", "/change-password"} {
		t.Run(path, func(t *testing.T) {
			s := newTempPasswordServer(t, true, 0)
			cookie := skippedSessionCookie(t, s, "ops", "Temp-Pass-9")
			before, _ := s.st.GetUserByUsername("ops")

			rec := doWithCookie(s, http.MethodPost, path, `{"new_password":"Another-Pass-2"}`, cookie)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current_password required") {
				t.Fatalf("no current password: status = %d body = %s, want 400 current_password required", rec.Code, rec.Body.String())
			}
			after, _ := s.st.GetUserByUsername("ops")
			if after.PasswordHash != before.PasswordHash {
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
			if rec := loginAs(t, s, "ops", "Another-Pass-2"); rec.Code != http.StatusOK {
				t.Errorf("login with the new password: status = %d, want 200", rec.Code)
			}
		})
	}
}

// A session that claims a forced change while the stored flag is already clear
// gets no waiver either.
func TestStaleForcedSessionNeedsCurrentPassword(t *testing.T) {
	s := newTempPasswordServer(t, false, 0)
	u, err := s.st.GetUserByUsername("ops")
	if err != nil {
		t.Fatal(err)
	}
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
	rec = doWithCookie(s, http.MethodPost, "/admin/change-password", `{"current_password":"Temp-Pass-9","new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("with current password: status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

// A non-admin account reaches change-password through the session-only route
// and gets the same rule.
func TestChangePasswordAfterSkipRequiresCurrentPasswordNonAdmin(t *testing.T) {
	s := newTempPasswordServer(t, false, 0)
	hash, _ := hashPassword("User-Pass-5")
	if _, err := s.st.CreateUser(store.User{
		Username: "viewer", Role: "user", Status: "active", PasswordHash: hash,
		MustChangePassword: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"viewer","password":"User-Pass-5"}`))
	login.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(loginRec, login)
	cookie := sessionCookieFrom(t, loginRec)
	rec := doWithCookie(s, http.MethodPost, "/skip-password-change", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("skip: status = %d body = %s", rec.Code, rec.Body.String())
	}
	cookie = sessionCookieFrom(t, rec)

	rec = doWithCookie(s, http.MethodPost, "/change-password", `{"new_password":"Another-Pass-2"}`, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current_password required") {
		t.Fatalf("no current password: status = %d body = %s, want 400 current_password required", rec.Code, rec.Body.String())
	}
}

// The forced first-login change keeps its waiver on every route.
func TestForcedSessionStillWaivesCurrentPassword(t *testing.T) {
	for _, path := range []string{"/admin/change-password", "/admin/v1/change-password", "/change-password"} {
		t.Run(path, func(t *testing.T) {
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
// affected by the session flag.
func TestLegacyEmptyUsernameChangePasswordUnchanged(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy-route.db"))
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
