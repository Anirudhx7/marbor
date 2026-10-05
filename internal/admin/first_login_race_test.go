package admin

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// barrierTimeout bounds the barrier wait so a failed test errors out instead of
// hanging.
const barrierTimeout = 10 * time.Second

// barrierStore holds every ChangeUserPassword call until n callers have
// arrived, so all concurrent requests have already passed authentication and
// read the same stored hash before any of them writes.
type barrierStore struct {
	store.Store
	n       int
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func (b *barrierStore) ChangeUserPassword(id int64, oldHash, newHash string) (bool, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-time.After(barrierTimeout):
		return false, errors.New("barrier: not all concurrent callers arrived")
	}
	return b.Store.ChangeUserPassword(id, oldHash, newHash)
}

// Several forced changes racing on the same account, each from its own valid
// session: exactly one is accepted, every other one is told the password was
// already changed (409), and the stored password is the winner's.
func TestConcurrentForcedPasswordChangeHasOneWinner(t *testing.T) {
	const n = 8
	s := newTempPasswordServerWith(t, true, 0, func(st store.Store) store.Store {
		return &barrierStore{Store: st, n: n, release: make(chan struct{})}
	})

	cookies := make([]*http.Cookie, n)
	for i := range cookies {
		cookies[i] = sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))
	}

	type result struct {
		pass string
		code int
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		pass := "Racing-Pass-" + string(rune('A'+i))
		cookie := cookies[i]
		go func() {
			rec := doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"`+pass+`"}`, cookie)
			results <- result{pass, rec.Code}
		}()
	}
	winners := []string{}
	for i := 0; i < n; i++ {
		r := <-results
		switch r.code {
		case http.StatusOK:
			winners = append(winners, r.pass)
		case http.StatusConflict:
		default:
			t.Errorf("request with %q: status = %d, want 200 or 409", r.pass, r.code)
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

// raceOnSkipStore performs a real password change just before the skip
// counter is bumped, simulating a change that lands between the handler's
// read and its write. This is a deterministic interleaving, not a real race:
// the change always lands at the same point, so the test never flakes.
type raceOnSkipStore struct {
	store.Store
	changeTo string
}

func (r *raceOnSkipStore) IncrementSkipCount(id int64, limit int) (bool, error) {
	u, err := r.Store.GetUserByID(id)
	if err != nil {
		return false, err
	}
	hash, err := hashPassword(r.changeTo)
	if err != nil {
		return false, err
	}
	if _, err := r.Store.ChangeUserPassword(id, u.PasswordHash, hash); err != nil {
		return false, err
	}
	return r.Store.IncrementSkipCount(id, limit)
}

// A skip racing a password change must lose: the new password stays intact and
// the caller is told no change is pending.
func TestSkipRacingPasswordChangeKeepsNewPassword(t *testing.T) {
	s := newTempPasswordServerWith(t, true, 0, func(st store.Store) store.Store {
		return &raceOnSkipStore{Store: st, changeTo: "Winner-Pass-1"}
	})
	cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))

	rec := doWithCookie(s, http.MethodPost, "/admin/skip-password-change", "", cookie)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_password_change_pending") {
		t.Fatalf("skip racing a change: status = %d body = %s, want 409 no_password_change_pending", rec.Code, rec.Body.String())
	}
	u, err := s.st.GetUserByUsername("ops")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(u.PasswordHash, "Winner-Pass-1") {
		t.Error("the changed password was overwritten by the skip")
	}
	if u.MustChangePassword || u.SkipPasswordCount != 0 {
		t.Errorf("mustChange=%v skip=%d, want false/0", u.MustChangePassword, u.SkipPasswordCount)
	}
}

// failSessionStore fails CreateUserSession once armed.
type failSessionStore struct {
	store.Store
	armed atomic.Bool
}

func (f *failSessionStore) CreateUserSession(sess store.UserSession) error {
	if f.armed.Load() {
		return errors.New("disk is full")
	}
	return f.Store.CreateUserSession(sess)
}

// If the new session cannot be stored after the password was saved, the caller
// gets a clear message to sign in again, the password really is changed, and
// the underlying error is not shown to the client.
func TestChangePasswordSessionFailureReportsPasswordChanged(t *testing.T) {
	buf := captureLog(t)
	var fs *failSessionStore
	s := newTempPasswordServerWith(t, true, 0, func(st store.Store) store.Store {
		fs = &failSessionStore{Store: st}
		return fs
	})
	cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))
	fs.armed.Store(true)

	rec := doWithCookie(s, http.MethodPost, "/admin/change-password", `{"new_password":"Fresh-Pass-77"}`, cookie)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "password changed; please sign in again") {
		t.Fatalf("status = %d body = %s, want 500 with the sign-in-again message", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "disk is full") {
		t.Error("internal error text reached the client")
	}
	u, err := s.st.GetUserByUsername("ops")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(u.PasswordHash, "Fresh-Pass-77") || u.MustChangePassword {
		t.Error("password was not saved before the session failure")
	}
	if !strings.Contains(buf.String(), "disk is full") || strings.Contains(buf.String(), "Fresh-Pass-77") {
		t.Errorf("log should hold the cause but never the password: %s", buf.String())
	}
}

// If the new session cannot be stored after a skip was recorded, the caller gets
// a static sign-in-again message and the cause stays in the server log only.
func TestSkipSessionFailureReportsSignInAgain(t *testing.T) {
	buf := captureLog(t)
	var fs *failSessionStore
	s := newTempPasswordServerWith(t, true, 0, func(st store.Store) store.Store {
		fs = &failSessionStore{Store: st}
		return fs
	})
	cookie := sessionCookieFrom(t, loginAs(t, s, "ops", "Temp-Pass-9"))
	fs.armed.Store(true)

	rec := doWithCookie(s, http.MethodPost, "/admin/skip-password-change", "", cookie)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "skip recorded; please sign in again") {
		t.Fatalf("status = %d body = %s, want 500 with the sign-in-again message", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "disk is full") {
		t.Error("internal error text reached the client")
	}
	if !strings.Contains(buf.String(), "disk is full") {
		t.Errorf("log should hold the cause: %s", buf.String())
	}
}

// The minimum length counts characters, not bytes, and the message tracks the
// shared constant.
func TestNewPasswordProblemCountsCharacters(t *testing.T) {
	if got := newPasswordProblem("ééééééé", ""); got != "password must be at least 8 characters" {
		t.Errorf("7 multibyte characters: got %q, want the length message", got)
	}
	if got := newPasswordProblem("éééééééé", ""); got != "" {
		t.Errorf("8 multibyte characters: got %q, want accepted", got)
	}
}
