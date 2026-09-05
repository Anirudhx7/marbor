package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityHeaders_SetsBaselineHeaders verifies baseline security
// headers are present on every response.
func TestSecurityHeaders_SetsBaselineHeaders(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := SecurityHeaders(inner)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	tests := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	}
	for header, want := range tests {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("Strict-Transport-Security header missing")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy header missing")
	}
}
