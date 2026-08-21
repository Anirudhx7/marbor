package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// Tests for the 2026-07-14 security audit fixes.

// TestEnsureAdminUser_DefaultCredentials verifies a fresh install creates a
// well-known admin/admin account (Priority 1: no generated secret to leak)
// with MustChangePassword forced, instead of a random logged password.
func TestEnsureAdminUser_DefaultCredentials(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "fresh.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	_ = NewServer(r, nil, config.Config{}, st) // ensureAdminUser runs in NewServer

	user, err := st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("GetUserByUsername(admin): %v", err)
	}
	if !user.MustChangePassword {
		t.Error("fresh admin account must have MustChangePassword = true")
	}
	if !verifyPassword(user.PasswordHash, defaultAdminPassword) {
		t.Error("fresh admin account password does not verify against the documented default")
	}
}

// TestLogin_SetsHttpOnlyCookie_NoTokenInBody verifies Priority 2: the session
// token is delivered only via an httpOnly cookie, never in the JSON body or
// localStorage-bound field.
func TestLogin_SetsHttpOnlyCookie_NoTokenInBody(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "login.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	body := bytes.NewReader([]byte(`{"username":"admin","password":"admin"}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/login", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no mesh_session cookie set on successful login")
	}
	if !found.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", found.SameSite)
	}
	if found.Value == "" {
		t.Error("session cookie value is empty")
	}

	var respBody map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if _, hasToken := respBody["token"]; hasToken {
		t.Error("login response body must not contain a token field")
	}
}

// TestLogout_ClearsCookieAndInvalidatesSession verifies logout expires the
// cookie and the old session token stops working afterward.
func TestLogout_ClearsCookieAndInvalidatesSession(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "logout.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	loginReq := httptest.NewRequest(http.MethodPost, "/admin/login",
		bytes.NewReader([]byte(`{"username":"admin","password":"admin"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginRec.Code)
	}
	var sessionCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("login did not set a session cookie")
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/admin/v1/logout", nil)
	logoutReq.AddCookie(sessionCookie)
	logoutRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(logoutRec, logoutReq)

	var cleared *http.Cookie
	for _, c := range logoutRec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatal("logout must expire the session cookie (MaxAge < 0)")
	}

	// The old session token must no longer authenticate anything.
	reuseReq := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	reuseReq.AddCookie(sessionCookie)
	reuseRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusUnauthorized {
		t.Errorf("request with post-logout session token = %d, want 401", reuseRec.Code)
	}
}

// fakeErrorStore wraps NopStore and forces AllModelConfigs to fail with an
// error containing details that must never reach the client (Priority 4).
type fakeErrorStore struct {
	store.NopStore
}

const leakySentinel = "dial tcp 10.1.2.3:5432: connect: connection refused (user=postgres password=hunter2)"

func (fakeErrorStore) AllModelConfigs() ([]store.ModelConfig, error) {
	return nil, fmt.Errorf(leakySentinel)
}

// TestModelConfigsError_NoRawLeak verifies a 5xx from a store failure never
// echoes the underlying error text - only a generic message + correlation ID.
func TestModelConfigsError_NoRawLeak(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, fakeErrorStore{})

	req := httptest.NewRequest(http.MethodGet, "/admin/model-configs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") || strings.Contains(body, "10.1.2.3") || strings.Contains(body, "postgres") {
		t.Errorf("response body leaks internal error detail: %s", body)
	}
	var decoded map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(decoded["error"], "request id") {
		t.Errorf("error body missing correlation id: %v", decoded)
	}
}

// TestLoginRateLimiter_TripsAtFivePerMinute verifies Priority 5's "5 attempts
// per minute per IP" login throttle.
func TestLoginRateLimiter_TripsAtFivePerMinute(t *testing.T) {
	l := newLoginRateLimiter()
	ip := "203.0.113.5"
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("attempt %d unexpectedly blocked before reaching the limit", i+1)
		}
		l.recordFailure(ip)
	}
	ok, retryAfter := l.allow(ip)
	if ok {
		t.Fatal("6th attempt within the window should be blocked")
	}
	if retryAfter <= 0 {
		t.Error("retryAfter should be positive once locked out")
	}
}

// TestResetPasswordRateLimiter_TripsAtThreePerHour verifies Priority 5's "3
// per hour per IP" password-reset throttle.
func TestResetPasswordRateLimiter_TripsAtThreePerHour(t *testing.T) {
	l := newResetPasswordRateLimiter()
	ip := "203.0.113.9"
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("attempt %d unexpectedly blocked before reaching the limit", i+1)
		}
		l.recordFailure(ip)
	}
	ok, retryAfter := l.allow(ip)
	if ok {
		t.Fatal("4th reset attempt within the hour should be blocked")
	}
	if retryAfter <= 0 || retryAfter > time.Hour {
		t.Errorf("retryAfter = %v, want a positive duration within the hour window", retryAfter)
	}
}
