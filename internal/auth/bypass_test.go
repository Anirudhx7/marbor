package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestAuthBypassRejection is a regression test for R4: auth must be exact-match
// only. A superstring, substring, or prefix of a valid key must all be rejected
// with 401. There was a historical bug where substring matching let attackers
// through - this test suite ensures it can never regress.
func TestAuthBypassRejection(t *testing.T) {
	const validKey = "sk-secret"

	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "regression-key", Key: validKey, RateLimit: 1000},
		},
	})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name string
		auth string
		want int
	}{
		// Control: exact valid key must succeed.
		{"exact valid key", "Bearer sk-secret", 200},

		// Superstring attacks - key is a prefix of the attacker token.
		{"superstring suffix -evil", "Bearer sk-secret-evil", 401},
		{"superstring suffix XYZ", "Bearer sk-secretXYZ", 401},

		// Substring/prefix attacks - attacker sends a prefix of the valid key.
		{"prefix sk-sec", "Bearer sk-sec", 401},
		{"prefix sk-", "Bearer sk-", 401},

		// Key embedded in a larger Bearer value (space-separated extra token).
		{"key plus extra token", "Bearer sk-secret extra", 401},

		// Missing or malformed Authorization header.
		{"empty bearer value", "Bearer ", 401},
		{"missing header entirely", "", 401},

		// Wrong scheme - raw key without Bearer prefix.
		{"raw key no Bearer", validKey, 401},

		// Totally different key to confirm general rejection still works.
		{"wrong key", "Bearer sk-wrong", 401},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("auth=%q: status = %d, want %d", tt.auth, rec.Code, tt.want)
			}
		})
	}
}

// TestAuthDisabledPassthrough confirms that when auth is disabled, all requests
// pass through regardless of the Authorization header value.
func TestAuthDisabledPassthrough(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(false)})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, auth := range []string{"", "Bearer garbage", "Bearer sk-anything"} {
		req := httptest.NewRequest("GET", "/", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("disabled auth, header=%q: got %d, want 200", auth, rec.Code)
		}
	}
}

// TestAddKeyAndRevokeBypass confirms that adding then revoking a key makes the
// exact token return 401 again, and that a superstring was never valid.
func TestAddKeyAndRevokeBypass(t *testing.T) {
	const key = "sk-dynamic"

	mw := NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	makeReq := func(bearer string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Before adding: both exact and superstring should be 401.
	if got := makeReq(key); got != 401 {
		t.Errorf("before add, exact key: got %d, want 401", got)
	}
	if got := makeReq(key + "-evil"); got != 401 {
		t.Errorf("before add, superstring: got %d, want 401", got)
	}

	mw.AddKey(config.KeyConfig{Name: "dyn", Key: key, RateLimit: 100})

	// After adding: exact must pass, superstring must still fail.
	if got := makeReq(key); got != 200 {
		t.Errorf("after add, exact key: got %d, want 200", got)
	}
	if got := makeReq(key + "-evil"); got != 401 {
		t.Errorf("after add, superstring: got %d, want 401", got)
	}

	mw.RevokeKey("dyn")

	// After revocation: exact must be 401 again.
	if got := makeReq(key); got != 401 {
		t.Errorf("after revoke, exact key: got %d, want 401", got)
	}
}
