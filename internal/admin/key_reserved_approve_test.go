package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func decodeErrorBody(t *testing.T, body string) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not a JSON error object: %q", body)
	}
	return m["error"]
}

func TestHandleApproveUser_RejectsReservedProbePrefixes(t *testing.T) {
	// The text must be identical to what POST /admin/keys returns.
	s0, _, _ := newFlakyKeyServer(t)
	createRec := keyReq(t, s0, http.MethodPost, "/admin/keys", `{"name":"benchmark-team"}`)
	if createRec.Code != http.StatusBadRequest {
		t.Fatalf("POST /admin/keys status = %d, want 400", createRec.Code)
	}
	wantMsg := decodeErrorBody(t, createRec.Body.String())

	cases := []struct {
		name     string
		username string
		body     string
	}{
		{"benchmark prefix", "carol", `{"create_key":{"name":"benchmark-team"}}`},
		{"pull-verify prefix", "carol", `{"create_key":{"name":"pull-verify-x"}}`},
		{"bare benchmark prefix", "carol", `{"create_key":{"name":"benchmark-"}}`},
		{"bare pull-verify prefix", "carol", `{"create_key":{"name":"pull-verify-"}}`},
		{"default name from username", "benchmark-bob", `{"create_key":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a, fs := newFlakyKeyServer(t)
			id := seedUser(t, s, tc.username, "pending", "")
			rec := keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/approve", id), tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := decodeErrorBody(t, rec.Body.String()); got != wantMsg {
				t.Errorf("error = %q, want identical to POST /admin/keys text %q", got, wantMsg)
			}
			if strings.Contains(rec.Body.String(), "sk-") || strings.Contains(rec.Body.String(), "api_key_value") {
				t.Errorf("response leaks key material: %s", rec.Body.String())
			}
			u, err := s.st.GetUserByID(id)
			if err != nil {
				t.Fatal(err)
			}
			if u.Status != "pending" {
				t.Errorf("user status = %q, want pending", u.Status)
			}
			keys, err := fs.AllKeys()
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 0 {
				t.Errorf("a key was persisted: %+v", keys)
			}
			if names := a.AllKeyNames(); len(names) != 0 {
				t.Errorf("a live key exists: %v", names)
			}
		})
	}
}

// Only the exact lowercase prefix at the start of the name is reserved;
// anything else is an ordinary operator key name.
func TestHandleApproveUser_AllowsNamesOutsideReservedPrefixes(t *testing.T) {
	for _, name := range []string{"team", "my-benchmark-key", "Benchmark-x"} {
		t.Run(name, func(t *testing.T) {
			s, a, _ := newFlakyKeyServer(t)
			id := seedUser(t, s, "dave", "pending", "")
			body := fmt.Sprintf(`{"create_key":{"name":%q}}`, name)
			rec := keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/approve", id), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if !hasLiveKey(a, name) {
				t.Errorf("key %q not live after approve", name)
			}
			u, _ := s.st.GetUserByID(id)
			if u.Status != "active" {
				t.Errorf("user status = %q, want active", u.Status)
			}
		})
	}
}
