package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Operator keys may not take a reserved probe-key name prefix, or the boot
// sweep of leftover probe keys could one day delete them.
func TestHandleAddKey_RejectsReservedProbePrefixes(t *testing.T) {
	s := newRealStoreTestServer(t)
	for _, name := range []string{"benchmark-team", "pull-verify-x"} {
		body := `{"name":"` + name + `","key":"sk-x"}`
		rec := httptest.NewRecorder()
		s.handleAddKey(rec, httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader([]byte(body))))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

// Only a name prefix is reserved: ordinary names and names that merely contain
// the text mid-string stay allowed.
func TestHandleAddKey_AllowsNamesThatOnlyContainProbeText(t *testing.T) {
	s := newRealStoreTestServer(t)
	for _, name := range []string{"team", "my-benchmark-key", "ci-pull-verify-x"} {
		body := `{"name":"` + name + `","key":"sk-` + name + `"}`
		rec := httptest.NewRecorder()
		s.handleAddKey(rec, httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader([]byte(body))))
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Errorf("%s: status = %d, want 2xx, body=%s", name, rec.Code, rec.Body.String())
		}
	}
}
