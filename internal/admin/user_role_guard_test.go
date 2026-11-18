package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// sessionFailStore fails DeleteUserSessionsByUserID while failSessions is set.
type sessionFailStore struct {
	store.Store
	mu           sync.Mutex
	failSessions bool
}

func (f *sessionFailStore) setFail(v bool) { f.mu.Lock(); f.failSessions = v; f.mu.Unlock() }

func (f *sessionFailStore) DeleteUserSessionsByUserID(id int64) error {
	f.mu.Lock()
	fail := f.failSessions
	f.mu.Unlock()
	if fail {
		return errors.New("injected session delete failure")
	}
	return f.Store.DeleteUserSessionsByUserID(id)
}

func newRoleGuardServer(t *testing.T) (*Server, *sessionFailStore) {
	t.Helper()
	real, err := store.Open(filepath.Join(t.TempDir(), "role-guard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { real.Close() })
	st := &sessionFailStore{Store: real}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	srv := NewServer(r, nil, config.Config{}, st)
	// NewServer seeds the default admin; remove it so each test controls the admin set.
	seeded, err := real.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("seeded admin missing: %v", err)
	}
	if err := real.SoftDeleteUser(seeded.ID, "test"); err != nil {
		t.Fatalf("SoftDeleteUser seeded admin: %v", err)
	}
	return srv, st
}

func addUser(t *testing.T, st store.Store, name, role, status string) int64 {
	t.Helper()
	id, err := st.CreateUser(store.User{Username: name, Role: role, Status: status, PasswordHash: "x"})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", name, err)
	}
	return id
}

func addSession(t *testing.T, st store.Store, token string, id int64, name, role string) {
	t.Helper()
	err := st.CreateUserSession(store.UserSession{
		Token: token, UserID: id, Role: role, Username: name, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}
}

func hasSession(t *testing.T, st store.Store, token string) bool {
	t.Helper()
	_, ok, err := st.GetUserSession(token)
	if err != nil {
		t.Fatalf("GetUserSession: %v", err)
	}
	return ok
}

func callUser(t *testing.T, h http.HandlerFunc, id int64, caller, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/admin/users/"+strconv.FormatInt(id, 10), strings.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUsername, caller))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func roleOf(t *testing.T, st store.Store, id int64) string {
	t.Helper()
	u, err := st.GetUserByID(id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	return u.Role
}

func TestPatchUserRoleChangeEndsSessions(t *testing.T) {
	s, st := newRoleGuardServer(t)
	root := addUser(t, st, "root", "admin", "active")
	other := addUser(t, st, "other", "admin", "active")
	_ = root
	addSession(t, st, "tok-other", other, "other", "admin")

	rec := callUser(t, s.handlePatchUser, other, "root", `{"role":"user"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote status = %d body=%s", rec.Code, rec.Body.String())
	}
	if hasSession(t, st, "tok-other") {
		t.Error("demoted user's session survived the role change")
	}
	if got := roleOf(t, st, other); got != "user" {
		t.Errorf("role = %q, want user", got)
	}
}

func TestPatchUserWithoutRoleChangeKeepsSessions(t *testing.T) {
	s, st := newRoleGuardServer(t)
	addUser(t, st, "root", "admin", "active")
	other := addUser(t, st, "other", "admin", "active")
	addSession(t, st, "tok-other", other, "other", "admin")

	for name, body := range map[string]string{
		"email only": `{"email":"o@example.com"}`,
		"same role":  `{"role":"admin"}`,
	} {
		rec := callUser(t, s.handlePatchUser, other, "root", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body=%s", name, rec.Code, rec.Body.String())
		}
		if !hasSession(t, st, "tok-other") {
			t.Errorf("%s: session was deleted without a role change", name)
		}
	}
}

func TestPatchUserSessionDeleteFailureIsRetryable(t *testing.T) {
	s, st := newRoleGuardServer(t)
	addUser(t, st, "root", "admin", "active")
	other := addUser(t, st, "other", "admin", "active")
	addSession(t, st, "tok-other", other, "other", "admin")

	st.setFail(true)
	rec := callUser(t, s.handlePatchUser, other, "root", `{"role":"user"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when sessions cannot be ended (body=%s)", rec.Code, rec.Body.String())
	}
	if got := roleOf(t, st, other); got != "admin" {
		t.Errorf("role = %q after a failed revoke, want unchanged admin so a retry still revokes", got)
	}

	st.setFail(false)
	rec = callUser(t, s.handlePatchUser, other, "root", `{"role":"user"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	if hasSession(t, st, "tok-other") {
		t.Error("session survived the retried role change")
	}
}

func TestLastActiveAdminGuards(t *testing.T) {
	s, st := newRoleGuardServer(t)
	a := addUser(t, st, "alpha", "admin", "active")
	b := addUser(t, st, "bravo", "admin", "suspended")
	c := addUser(t, st, "charlie", "admin", "pending")
	_, _ = b, c

	if rec := callUser(t, s.handlePatchUser, a, "alpha", `{"role":"user"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("self-demote of the only active admin: status = %d, want 400", rec.Code)
	}
	if got := roleOf(t, st, a); got != "admin" {
		t.Errorf("role = %q after a blocked demote, want admin", got)
	}
	if rec := callUser(t, s.handleDeleteUser, a, "someone", ``); rec.Code != http.StatusBadRequest {
		t.Errorf("delete of the only active admin: status = %d, want 400", rec.Code)
	}
	if rec := callUser(t, s.handleDeleteUser, a, "alpha", ``); rec.Code != http.StatusBadRequest {
		t.Errorf("self-delete of the only active admin: status = %d, want 400", rec.Code)
	}
	if _, err := st.GetUserByID(a); err != nil {
		t.Errorf("only active admin vanished: %v", err)
	}
}

func TestInactiveAdminsCanStillBeDemotedAndDeleted(t *testing.T) {
	s, st := newRoleGuardServer(t)
	addUser(t, st, "alpha", "admin", "active")
	b := addUser(t, st, "bravo", "admin", "suspended")
	c := addUser(t, st, "charlie", "admin", "pending")

	if rec := callUser(t, s.handlePatchUser, b, "alpha", `{"role":"user"}`); rec.Code != http.StatusOK {
		t.Errorf("demote suspended admin: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if rec := callUser(t, s.handleDeleteUser, c, "alpha", ``); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Errorf("delete pending admin: status = %d body=%s, want success", rec.Code, rec.Body.String())
	}
}

func TestGuardFailsClosedWhenAdminCountFails(t *testing.T) {
	s, st := newRoleGuardServer(t)
	a := addUser(t, st, "alpha", "admin", "active")
	addUser(t, st, "beta", "admin", "active")
	s.st = countErrStore{s.st}

	if rec := callUser(t, s.handlePatchUser, a, "beta", `{"role":"user"}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("demote with failing count: status = %d, want 500", rec.Code)
	}
	if got := roleOf(t, st, a); got != "admin" {
		t.Errorf("role = %q, want admin", got)
	}
}

type countErrStore struct{ store.Store }

func (countErrStore) CountAdminUsers() (int, error) { return 0, errors.New("injected count failure") }

func TestConcurrentMutualDemotionKeepsOneActiveAdmin(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, st := newRoleGuardServer(t)
		a := addUser(t, st, "alpha", "admin", "active")
		b := addUser(t, st, "bravo", "admin", "active")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); callUser(t, s.handlePatchUser, a, "bravo", `{"role":"user"}`) }()
		go func() { defer wg.Done(); callUser(t, s.handlePatchUser, b, "alpha", `{"role":"user"}`) }()
		wg.Wait()

		n, err := st.CountAdminUsers()
		if err != nil {
			t.Fatalf("CountAdminUsers: %v", err)
		}
		if n != 1 {
			t.Fatalf("round %d: %d active admins after concurrent mutual demotion, want 1", i, n)
		}
	}
}

func TestPatchUserResponseStillEncodesUser(t *testing.T) {
	s, st := newRoleGuardServer(t)
	addUser(t, st, "root", "admin", "active")
	other := addUser(t, st, "other", "user", "active")
	rec := callUser(t, s.handlePatchUser, other, "root", `{"role":"admin"}`)
	var u store.User
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil || u.Role != "admin" {
		t.Errorf("response user = %+v err=%v, want role admin", u, err)
	}
}

func TestSuspendLastActiveAdminIsBlocked(t *testing.T) {
	s, st := newRoleGuardServer(t)
	a := addUser(t, st, "alpha", "admin", "active")
	addUser(t, st, "bravo", "admin", "suspended")

	if rec := callUser(t, s.handleSuspendUser, a, "someone", ``); rec.Code != http.StatusBadRequest {
		t.Fatalf("suspend of the only active admin: status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	u, err := st.GetUserByID(a)
	if err != nil || u.Status != "active" {
		t.Errorf("admin status = %q err=%v after a blocked suspend, want active", u.Status, err)
	}
}

func TestConcurrentSuspendAndDemoteKeepOneActiveAdmin(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, st := newRoleGuardServer(t)
		a := addUser(t, st, "alpha", "admin", "active")
		b := addUser(t, st, "bravo", "admin", "active")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); callUser(t, s.handlePatchUser, a, "bravo", `{"role":"user"}`) }()
		go func() { defer wg.Done(); callUser(t, s.handleSuspendUser, b, "alpha", ``) }()
		wg.Wait()

		n, err := st.CountAdminUsers()
		if err != nil {
			t.Fatalf("CountAdminUsers: %v", err)
		}
		if n != 1 {
			t.Fatalf("round %d: %d active admins after concurrent suspend and demote, want 1", i, n)
		}
	}
}
