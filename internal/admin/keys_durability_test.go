package admin

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// flakyKeyStore wraps a real store and fails key writes/reads on demand.
type flakyKeyStore struct {
	store.Store
	failUpsert, failRevoke, failAll bool
}

var errInjected = http.ErrAbortHandler

func (f *flakyKeyStore) UpsertKey(k store.KeyRecord) error {
	if f.failUpsert {
		return errInjected
	}
	return f.Store.UpsertKey(k)
}

func (f *flakyKeyStore) RevokeKey(n string) error {
	if f.failRevoke {
		return errInjected
	}
	return f.Store.RevokeKey(n)
}

func (f *flakyKeyStore) AllKeys() ([]store.KeyRecord, error) {
	if f.failAll {
		return nil, errInjected
	}
	return f.Store.AllKeys()
}

func newFlakyKeyServer(t *testing.T) (*Server, *auth.Middleware, *flakyKeyStore) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fs := &flakyKeyStore{Store: st}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	a := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	s := NewServer(r, a, config.Config{}, fs)
	t.Cleanup(s.Shutdown)
	return s, a, fs
}

func keyReq(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func hasLiveKey(a *auth.Middleware, name string) bool {
	for _, n := range a.AllKeyNames() {
		if n == name {
			return true
		}
	}
	return false
}

func TestAdmin_AddKeyStoreFailureLeavesLiveAuthUnchanged(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	fs.failUpsert = true
	rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if hasLiveKey(a, "k1") {
		t.Error("key is live although its durable write failed")
	}
}

func TestAdmin_RotateKeyStoreFailureKeepsOldToken(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1","key":"sk-old-token"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	fs.failUpsert = true
	rec := keyReq(t, s, http.MethodPost, "/admin/keys/k1/rotate", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	ok := a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-old-token")
	out := httptest.NewRecorder()
	ok.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Errorf("old token rejected (%d) after failed rotate", out.Code)
	}
}

func TestAdmin_RevokeKeyStoreFailureIsNot204AndKeyStaysLive(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	fs.failRevoke = true
	rec := keyReq(t, s, http.MethodDelete, "/admin/keys/k1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !hasLiveKey(a, "k1") {
		t.Error("live key was removed although the durable revoke failed")
	}
}

func TestAdmin_PatchKeyStoreFailureIs500AndLiveUnchanged(t *testing.T) {
	for _, mode := range []string{"allkeys", "upsert"} {
		t.Run(mode, func(t *testing.T) {
			s, a, fs := newFlakyKeyServer(t)
			if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1","rateLimit":5}`); rec.Code != http.StatusCreated {
				t.Fatalf("create = %d", rec.Code)
			}
			fs.failAll = mode == "allkeys"
			fs.failUpsert = mode == "upsert"
			rec := keyReq(t, s, http.MethodPatch, "/admin/keys/k1", `{"rate_limit":99}`)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			_, _, _, _, _, rl, _, _ := a.KeyStats("k1")
			if rl != 5 {
				t.Errorf("live rate limit = %d, want 5 (unchanged)", rl)
			}
		})
	}
}

// The dashboard sends snake_case rate_limit/expires_at on create; both
// spellings must be honored and stored.
func TestAdmin_AddKeyAcceptsSnakeCaseRateLimitAndExpiry(t *testing.T) {
	s, a, _ := newFlakyKeyServer(t)
	rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1","rate_limit":42,"expires_at":"2099-01-01"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	_, _, _, _, exp, rl, _, ok := a.KeyStats("k1")
	if !ok || rl != 42 || exp != "2099-01-01" {
		t.Errorf("live key rl=%d exp=%q ok=%v, want 42 / 2099-01-01", rl, exp, ok)
	}
	recs, _ := s.st.AllKeys()
	if len(recs) != 1 || recs[0].RateLimit != 42 || recs[0].ExpiresAt != "2099-01-01" {
		t.Errorf("stored = %+v", recs)
	}
	rec = keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k2","rate_limit":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative snake_case rate_limit = %d, want 400", rec.Code)
	}
	rec = keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k3","rateLimit":7,"expiresAt":"2099-01-01"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("camelCase create = %d", rec.Code)
	}
	if _, _, _, _, exp, rl, _, _ := a.KeyStats("k3"); rl != 7 || exp != "2099-01-01" {
		t.Errorf("camelCase key rl=%d exp=%q", rl, exp)
	}
}
