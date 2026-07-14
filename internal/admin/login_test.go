package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// loginReq POSTs credentials to handleLogin from the given remote address and
// returns the response. remoteAddr must be in host:port form to exercise the
// same IP-extraction path as production traffic.
func loginReq(s *Server, username, password, remoteAddr string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)
	return rec
}

func TestLoginRateLimiter_LocksOutAfterThreshold(t *testing.T) {
	s := newTestServer()
	s.SetDemoMode(true)

	ip := "10.0.0.1:5000"

	// 5 failed attempts should each return 401 (not yet locked out).
	for i := 0; i < 5; i++ {
		rec := loginReq(s, "admin", "wrong-password", ip)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got status %d, want 401", i+1, rec.Code)
		}
	}

	// 6th attempt (still wrong, but now over threshold) must be blocked with 429.
	rec := loginReq(s, "admin", "wrong-password", ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got status %d, want 429 after exceeding threshold", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 lockout response")
	}

	// Even the CORRECT credentials must now be rejected  --  the IP is locked out,
	// not just the bad guesses.
	rec = loginReq(s, "admin", "admin", ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct credentials during lockout: got status %d, want 429", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected an error message in the lockout response")
	}
	// Must not leak whether the username exists.
	if bytes.Contains(rec.Body.Bytes(), []byte("admin")) {
		t.Errorf("lockout response should not mention the username: %s", rec.Body.String())
	}
}

func TestLoginRateLimiter_DifferentIPsIndependent(t *testing.T) {
	s := newTestServer()
	s.SetDemoMode(true)

	// Hammer one IP into lockout.
	for i := 0; i < 6; i++ {
		loginReq(s, "admin", "wrong-password", "10.0.0.2:5000")
	}

	// A different IP must be unaffected.
	rec := loginReq(s, "admin", "admin", "10.0.0.3:6000")
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated IP got status %d, want 200", rec.Code)
	}
}

func TestLoginRateLimiter_SuccessResetsFailureCount(t *testing.T) {
	s := newTestServer()
	s.SetDemoMode(true)

	ip := "10.0.0.4:5000"

	// A few failures, but under the threshold.
	for i := 0; i < 4; i++ {
		loginReq(s, "admin", "wrong-password", ip)
	}

	// A successful login should clear the failure count.
	rec := loginReq(s, "admin", "admin", ip)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected successful login, got status %d", rec.Code)
	}

	// Failures should have been reset  --  4 more wrong attempts should still be
	// under threshold (not locked out), proving the counter didn't carry over.
	for i := 0; i < 4; i++ {
		rec := loginReq(s, "admin", "wrong-password", ip)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d after reset: got 429, want 401 (counter should have reset on success)", i+1)
		}
	}
}

func TestLoginRateLimiter_UnlocksAfterLockDuration(t *testing.T) {
	l := newLoginRateLimiter()
	l.maxAttempts = 3
	l.window = time.Minute
	l.lockDuration = 10 * time.Millisecond

	ip := "10.0.0.5"
	for i := 0; i < 3; i++ {
		l.recordFailure(ip)
	}

	if ok, _ := l.allow(ip); ok {
		t.Fatal("expected IP to be locked out immediately after threshold")
	}

	time.Sleep(20 * time.Millisecond)

	if ok, _ := l.allow(ip); !ok {
		t.Fatal("expected IP to be unlocked after lockDuration elapsed")
	}
}

func TestClientIP_StripsPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "192.168.1.10:54321"
	if got := clientIP(req); got != "192.168.1.10" {
		t.Errorf("clientIP() = %q, want %q", got, "192.168.1.10")
	}
}

func TestClientIP_FallsBackOnMalformedAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "not-a-host-port"
	if got := clientIP(req); got != "not-a-host-port" {
		t.Errorf("clientIP() = %q, want fallback %q", got, "not-a-host-port")
	}
}
