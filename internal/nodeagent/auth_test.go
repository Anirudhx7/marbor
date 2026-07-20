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
