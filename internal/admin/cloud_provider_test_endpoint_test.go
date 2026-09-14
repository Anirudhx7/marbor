package admin

// cloud_provider_test_endpoint_test.go - regression coverage for
// handleTestCloudProvider's error handling: 9aff1df (fix(admin): stop
// leaking raw dial/TLS errors to cloud-test and probe clients) and
// 5abd358 (fix(admin): map cloud-provider auth rejection to 502 instead of
// 400).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleTestCloudProvider_DoesNotLeakRawDialError is the regression test
// for 9aff1df. Before that fix, a failed dial to base_url returned
// err.Error() straight to the client via writeJSONError, exposing host:port
// and dial-failure detail. The fix routes it through writeCorrelatedError:
// a generic message plus a correlation ID, with the real error logged
// server-side only. This uses an address nothing listens on
// (127.0.0.1:1, a reserved/unassigned port) to force a real dial failure.
func TestHandleTestCloudProvider_DoesNotLeakRawDialError(t *testing.T) {
	s := newRealStoreTestServer(t)

	const unreachable = "http://127.0.0.1:1"
	body := `{"provider":"openai","base_url":"` + unreachable + `","api_key":"sk-test"}`
	rec := httptest.NewRecorder()
	s.handleTestCloudProvider(rec, httptest.NewRequest(http.MethodPost, "/admin/cloud/providers/test", bytes.NewReader([]byte(body))))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(out.Error, "127.0.0.1:1") {
		t.Errorf("response leaked the raw dial target %q: %q", unreachable, out.Error)
	}
	if strings.Contains(strings.ToLower(out.Error), "connect") || strings.Contains(strings.ToLower(out.Error), "refused") {
		t.Errorf("response leaked raw dial-error detail: %q", out.Error)
	}
	if out.Error == "" || !strings.Contains(out.Error, "request id") {
		t.Errorf("response = %q, want a generic message with a correlation ID (request id: ...)", out.Error)
	}
}

// TestHandleTestCloudProvider_MapsProviderAuthRejectionTo502 is the
// regression test for 5abd358. Before that fix, a 401/403 from the cloud
// provider under test was remapped to admin-facing 400 to work around a UI
// bug where apiFetch force-logs-out the operator on any 401 from any admin
// endpoint. That masked a real credential rejection as a client-request
// validation error. The fix returns 502 instead, since apiFetch only
// special-cases exactly 401.
func TestHandleTestCloudProvider_MapsProviderAuthRejectionTo502(t *testing.T) {
	s := newRealStoreTestServer(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	body := `{"provider":"openai","base_url":"` + upstream.URL + `","api_key":"sk-rejected"}`
	rec := httptest.NewRecorder()
	s.handleTestCloudProvider(rec, httptest.NewRequest(http.MethodPost, "/admin/cloud/providers/test", bytes.NewReader([]byte(body))))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (provider auth rejection must not surface as 400, which the old code did to dodge the UI's 401 force-logout)", rec.Code)
	}
}
