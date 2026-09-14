package admin

// key_lifecycle_test.go - regression coverage for 571c9b2 (fix(admin):
// reject duplicate names on key create, add explicit rotate).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleAddKey_RejectsDuplicateName is the regression test for 571c9b2.
// Before that fix, POST /admin/keys had no duplicate-name check: a repeat
// POST for an existing name silently rotated the key via auth.AddKey +
// store UpsertKey and handed back a fresh plaintext secret for what the
// caller thought was a create, killing the old key fleet-wide with no
// warning. The second POST for the same name must now be rejected with 409
// and leave the original key's token untouched.
func TestHandleAddKey_RejectsDuplicateName(t *testing.T) {
	s := newRealStoreTestServer(t)

	first := `{"name":"dup-key","key":"sk-original-token","rateLimit":1000}`
	rec := httptest.NewRecorder()
	s.handleAddKey(rec, httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader([]byte(first))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	second := `{"name":"dup-key","key":"sk-attacker-or-retry-token","rateLimit":1000}`
	rec2 := httptest.NewRecorder()
	s.handleAddKey(rec2, httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader([]byte(second))))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate-name create: status = %d, want 409, body=%s", rec2.Code, rec2.Body.String())
	}
	if loc := rec2.Header().Get("Location"); loc == "" {
		t.Error("409 response missing Location header pointing at the existing key")
	}

	keys, err := s.st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("len(keys) = %d, want 1 (duplicate POST must not create a second record)", len(keys))
	}
	if keys[0].Key != "sk-original-token" {
		t.Errorf("stored key token = %q after rejected duplicate create, want unchanged %q (silent rotation regression)", keys[0].Key, "sk-original-token")
	}
}

// TestHandleRotateKey_IssuesNewTokenPreservingPolicy verifies the explicit
// rotate endpoint 571c9b2 introduced as the only deliberate way to reissue a
// key's token: it must replace the token while preserving the key's policy
// fields (here, RateLimit).
func TestHandleRotateKey_IssuesNewTokenPreservingPolicy(t *testing.T) {
	s := newRealStoreTestServer(t)

	create := `{"name":"rotate-key","key":"sk-before-rotate","rateLimit":42}`
	rec := httptest.NewRecorder()
	s.handleAddKey(rec, httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader([]byte(create))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/keys/rotate-key/rotate", nil)
	req.SetPathValue("name", "rotate-key")
	rec2 := httptest.NewRecorder()
	s.handleRotateKey(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d, want 200, body=%s", rec2.Code, rec2.Body.String())
	}

	var rotated struct {
		Key       string `json:"key"`
		RateLimit int    `json:"rateLimit"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&rotated); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if rotated.Key == "" || rotated.Key == "sk-before-rotate" {
		t.Errorf("rotated key token = %q, want a fresh, non-empty token", rotated.Key)
	}
	if rotated.RateLimit != 42 {
		t.Errorf("rotated key RateLimit = %d, want 42 (policy fields must be preserved)", rotated.RateLimit)
	}

	keys, err := s.st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Key != rotated.Key {
		t.Fatalf("stored keys after rotate = %+v, want single key matching rotated token %q", keys, rotated.Key)
	}
}
