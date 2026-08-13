package nodeagent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckTokenExactMatch(t *testing.T) {
	cases := []struct {
		name       string
		authHeader string
		expected   string
		want       bool
	}{
		{"exact match", "Bearer secret123", "secret123", true},
		{"wrong token", "Bearer wrong", "secret123", false},
		// TrimPrefix is a no-op when the prefix isn't present, so a raw token
		// with no "Bearer " prefix still matches - identical to R4's admin
		// auth behavior (strings.TrimPrefix(authHeader, "Bearer ") == token).
		{"missing Bearer prefix still matches (same as R4)", "secret123", "secret123", true},
		{"empty header", "", "secret123", false},
		// R8 lesson, applied to the agent side of this trust boundary: an
		// agent with no configured token must reject EVERY request,
		// including one whose bearer value happens to be empty (trailing
		// space, "Authorization: Bearer "), not fall open.
		{"empty expected token, empty bearer value", "Bearer ", "", false},
		{"empty expected token, real bearer value", "Bearer secret123", "", false},
		{"empty expected token, no header at all", "", "", false},
		// A substring/contains-style match must not pass - only exact match.
		{"token is a substring of header", "Bearer secret123extra", "secret123", false},
		{"expected token is a substring of provided", "Bearer secret1", "secret123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkToken(tc.authHeader, tc.expected)
			if got != tc.want {
				t.Errorf("checkToken(%q, %q) = %v, want %v", tc.authHeader, tc.expected, got, tc.want)
			}
		})
	}
}

func TestRequireTokenMiddleware(t *testing.T) {
	handlerCalled := false
	h := requireToken("mytoken", func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	t.Run("valid token passes through", func(t *testing.T) {
		handlerCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		req.Header.Set("Authorization", "Bearer mytoken")
		w := httptest.NewRecorder()
		h(w, req)
		if !handlerCalled {
			t.Error("handler was not called with a valid token")
		}
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("invalid token rejected with 401", func(t *testing.T) {
		handlerCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		h(w, req)
		if handlerCalled {
			t.Error("handler was called despite an invalid token")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})

	t.Run("missing token rejected with 401", func(t *testing.T) {
		handlerCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		w := httptest.NewRecorder()
		h(w, req)
		if handlerCalled {
			t.Error("handler was called despite a missing token")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})
}

// TestRequireTokenNeverAuthenticatesEmptyExpected verifies that an agent
// configured with no token (expectedToken == "") rejects every request,
// including a blank bearer value - it must never fall open.
func TestRequireTokenNeverAuthenticatesEmptyExpected(t *testing.T) {
	h := requireToken("", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must never be called when no token is configured")
	})
	for _, authHeader := range []string{"", "Bearer ", "Bearer anything"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("authHeader=%q: status = %d, want 401", authHeader, w.Code)
		}
	}
}

// TestScopeOf verifies token scope parsing (P54): recognized "<tier>."
// prefixes parse to their tier, and anything else - including every
// pre-P54 token, which is a bare random string with no "." at all - falls
// back to tierAdmin (full scope), the deliberate backward-compat path.
func TestScopeOf(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  tier
	}{
		{"readonly prefix", "readonly.Xk9fA1b2C3d4", tierReadonly},
		{"operator prefix", "operator.Xk9fA1b2C3d4", tierOperator},
		{"admin prefix", "admin.Xk9fA1b2C3d4", tierAdmin},
		{"legacy token with no prefix at all falls back to admin", "Xk9fA1b2C3d4NoDotHere", tierAdmin},
		{"unrecognized prefix word falls back to admin", "superuser.Xk9fA1b2C3d4", tierAdmin},
		{"empty token falls back to admin", "", tierAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeOf(tc.token); got != tc.want {
				t.Errorf("scopeOf(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// TestTokenScope verifies the exported TokenScope wrapper (used by admin.go
// and its tests) agrees with scopeOf's internal tier parsing.
func TestTokenScope(t *testing.T) {
	cases := []struct {
		token string
		want  string
	}{
		{"readonly.secret", ScopeReadonly},
		{"operator.secret", ScopeOperator},
		{"admin.secret", ScopeAdmin},
		{"legacynoprefix", ScopeAdmin},
	}
	for _, tc := range cases {
		if got := TokenScope(tc.token); got != tc.want {
			t.Errorf("TokenScope(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

// TestRequireScopeAuthorizesActionAAndRejectsActionB is the core P54
// regression: a token issued for one action must succeed for that action
// (A) and be rejected, not silently pass, for a different action requiring
// a higher tier (B) - the security invariant the whole feature exists to
// enforce.
func TestRequireScopeAuthorizesActionAAndRejectsActionB(t *testing.T) {
	const token = "readonly.Xk9fA1b2C3d4"
	handlerCalled := false
	resetCalled := func() { handlerCalled = false }
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}

	t.Run("action A: readonly token authorized for a readonly-tier route", func(t *testing.T) {
		resetCalled()
		h := requireScope(token, tierReadonly, okHandler)
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h(w, req)
		if !handlerCalled {
			t.Error("handler was not called for an authorized action")
		}
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("action B: same readonly token rejected for an operator-tier route", func(t *testing.T) {
		resetCalled()
		h := requireScope(token, tierOperator, okHandler)
		req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h(w, req)
		if handlerCalled {
			t.Error("handler was called despite insufficient scope")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
}

// TestRequireScopeFutureAdminActionNeverFallsOpen proves that a token valid
// for today's highest-in-use tier (operator) cannot reach a route requiring
// tierAdmin merely by being a valid token - this stands in for "a future
// Group 3 action" that doesn't exist in the route table yet, since the
// check is purely ordinal and doesn't special-case any specific route.
func TestRequireScopeFutureAdminActionNeverFallsOpen(t *testing.T) {
	const token = "operator.Xk9fA1b2C3d4"
	h := requireScope(token, tierAdmin, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must never be called: operator-tier token must not reach an admin-tier action")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/runtime/upgrade", nil) // hypothetical future Group 3 route
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// TestRequireScopeWrongTokenStillRejectedBefore401 verifies scope checking
// never runs before the bearer match: an invalid/missing token is always
// 401, never 403, regardless of what tier the route requires.
func TestRequireScopeWrongTokenStillRejectedBefore401(t *testing.T) {
	const expected = "operator.Xk9fA1b2C3d4"
	h := requireScope(expected, tierReadonly, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must never be called with an invalid token")
	})

	t.Run("wrong token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})
}
