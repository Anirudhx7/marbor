package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// flakyKeyStore wraps a real store and fails key writes/reads on demand.
type flakyKeyStore struct {
	store.Store
	failUpsert, failRevoke, failAll, failUpdateUser, failDelete bool
	// afterUpsert runs after each successful UpsertKey (test hook).
	afterUpsert func()
}

var errInjected = errors.New("injected store failure")

func (f *flakyKeyStore) UpsertKey(k store.KeyRecord) error {
	if f.failUpsert {
		return errInjected
	}
	err := f.Store.UpsertKey(k)
	if err == nil && f.afterUpsert != nil {
		f.afterUpsert()
	}
	return err
}

func (f *flakyKeyStore) DeleteKey(n string) error {
	if f.failDelete {
		return errInjected
	}
	return f.Store.DeleteKey(n)
}

func (f *flakyKeyStore) RevokeKey(n string) error {
	if f.failRevoke {
		return errInjected
	}
	return f.Store.RevokeKey(n)
}

func (f *flakyKeyStore) UpdateUser(u store.User) error {
	if f.failUpdateUser {
		return errInjected
	}
	return f.Store.UpdateUser(u)
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
	s.AdminToken() // enables the test session once, before any concurrent request
	return s, a, fs
}

func keyReq(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "demo-session"})
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

func TestAdmin_RevokeKeyStoreFailureIs500ButLiveKeyIsRevoked(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	fs.failRevoke = true
	rec := keyReq(t, s, http.MethodDelete, "/admin/keys/k1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if hasLiveKey(a, "k1") {
		t.Error("live key still authenticates although revoke was requested; a failed durable write must not undo the revocation")
	}
	if !strings.Contains(rec.Body.String(), "restart") {
		t.Errorf("error should say the key may return after a restart: %s", rec.Body.String())
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

func seedUser(t *testing.T, s *Server, name, status, keyName string) int64 {
	t.Helper()
	id, err := s.st.CreateUser(store.User{Username: name, Role: "user", Status: status, APIKeyName: keyName})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAdmin_SuspendAndDeleteUserRevokeStoreFailureIs500ButKeyRevoked(t *testing.T) {
	for _, op := range []string{"suspend", "delete"} {
		t.Run(op, func(t *testing.T) {
			s, a, fs := newFlakyKeyServer(t)
			if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1"}`); rec.Code != http.StatusCreated {
				t.Fatalf("create = %d", rec.Code)
			}
			id := seedUser(t, s, "bob", "active", "k1")
			fs.failRevoke = true
			var rec *httptest.ResponseRecorder
			if op == "suspend" {
				rec = keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/suspend", id), "")
			} else {
				rec = keyReq(t, s, http.MethodDelete, fmt.Sprintf("/admin/v1/users/%d", id), "")
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
			}
			if hasLiveKey(a, "k1") {
				t.Error("live key still authenticates after the user was removed")
			}
		})
	}
}

func TestAdmin_ApproveUserKeyStoreFailureLeavesUserPending(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	id := seedUser(t, s, "carol", "pending", "")
	fs.failUpsert = true
	rec := keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/approve", id), `{"create_key":{"name":"ck"}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if hasLiveKey(a, "ck") {
		t.Error("key is live although its durable write failed")
	}
	u, _ := s.st.GetUserByID(id)
	if u.Status != "pending" {
		t.Errorf("user status = %q, want pending", u.Status)
	}
}

func TestAdmin_ApproveUserUpdateFailureRemovesCreatedKey(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	id := seedUser(t, s, "dave", "pending", "")
	fs.failUpdateUser = true
	rec := keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/approve", id), `{"create_key":{"name":"dk"}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if hasLiveKey(a, "dk") {
		t.Error("orphan key left live after the approval failed")
	}
	if recs, _ := s.st.AllKeys(); len(recs) != 0 {
		t.Errorf("orphan key left in store: %+v", recs)
	}
}

func TestAdmin_VerifyModelLoadsKeyStoreFailureIsReported(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	s.cfg.Proxy.Port = 11434
	fs.failUpsert = true
	if err := s.verifyModelLoads(context.Background(), "n1", "m1"); err == nil {
		t.Fatal("expected an error when the verification key cannot be persisted")
	}
	if len(a.AllKeyNames()) != 0 {
		t.Errorf("verification key is live although it was not persisted: %v", a.AllKeyNames())
	}
}

// Every KeyConfig field the dashboard sends in snake_case must be honored.
func TestAdmin_AddKeyAcceptsSnakeCaseForEveryField(t *testing.T) {
	s, a, _ := newFlakyKeyServer(t)
	rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1","daily_limit":11,"monthly_limit":22,"daily_usd_cap":1.5,"monthly_usd_cap":2.5,"local_only":true,"allow_local_degradation":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	recs, _ := s.st.AllKeys()
	if len(recs) != 1 {
		t.Fatalf("stored = %+v", recs)
	}
	g := recs[0]
	if d, m, _ := a.KeyRequestLimits("k1"); d != 11 || m != 22 {
		t.Errorf("live request limits = %d/%d, want 11/22", d, m)
	}
	if d, m, _ := a.KeyUsdCaps("k1"); d != 1.5 || m != 2.5 {
		t.Errorf("live usd caps = %v/%v, want 1.5/2.5", d, m)
	}
	if !a.IsLocalOnly("k1") || !a.IsAllowLocalDegradation("k1") {
		t.Error("live local_only / allow_local_degradation not applied")
	}
	if g.DailyLimit != 11 || g.MonthlyLimit != 22 || g.DailyUsdCap != 1.5 || g.MonthlyUsdCap != 2.5 || !g.LocalOnly || !g.AllowLocalDegradation {
		t.Errorf("snake_case fields dropped: %+v", g)
	}
	for _, f := range []string{"daily_limit", "monthly_limit", "daily_usd_cap", "monthly_usd_cap"} {
		rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"neg","`+f+`":-1}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("negative %s = %d, want 400", f, rec.Code)
		}
	}
}

func TestAdmin_RotateRacingRevokeNeverResurrectsKey(t *testing.T) {
	for i := 0; i < 25; i++ {
		s, a, _ := newFlakyKeyServer(t)
		if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1"}`); rec.Code != http.StatusCreated {
			t.Fatalf("create = %d", rec.Code)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); keyReq(t, s, http.MethodPost, "/admin/keys/k1/rotate", "") }()
		go func() { defer wg.Done(); keyReq(t, s, http.MethodDelete, "/admin/keys/k1", "") }()
		wg.Wait()
		// A rotate that lands after the revoke must be refused, so the key can
		// never end up live again.
		if hasLiveKey(a, "k1") {
			t.Fatalf("iteration %d: key is live after a revoke raced a rotate", i)
		}
	}
}

func TestAdmin_ConcurrentSameNameCreateYieldsOneCreatedOneConflict(t *testing.T) {
	for i := 0; i < 15; i++ {
		s, _, _ := newFlakyKeyServer(t)
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes <- keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"dup"}`).Code
			}()
		}
		wg.Wait()
		close(codes)
		created, conflict := 0, 0
		for c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflict++
			}
		}
		if created != 1 || conflict != 1 {
			t.Fatalf("iteration %d: created=%d conflict=%d, want 1 and 1", i, created, conflict)
		}
	}
}

func TestAdmin_ApproveUserKeyNameCollisionIs409AndKeepsExistingKey(t *testing.T) {
	s, a, _ := newFlakyKeyServer(t)
	if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"ck","key":"sk-existing"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	id := seedUser(t, s, "erin", "pending", "")
	rec := keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/approve", id), `{"create_key":{"name":"ck"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if u, _ := s.st.GetUserByID(id); u.Status != "pending" {
		t.Errorf("user status = %q, want pending", u.Status)
	}
	if !hasLiveKey(a, "ck") {
		t.Error("pre-existing key was removed")
	}
	recs, _ := s.st.AllKeys()
	if len(recs) != 1 || recs[0].Key != "sk-existing" {
		t.Errorf("stored keys = %+v, want the original key untouched", recs)
	}
}

func TestAdmin_ApproveUserRollbackDeleteFailureStillRevokesKey(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	id := seedUser(t, s, "frank", "pending", "")
	fs.failUpdateUser = true
	fs.failDelete = true
	rec := keyReq(t, s, http.MethodPost, fmt.Sprintf("/admin/v1/users/%d/approve", id), `{"create_key":{"name":"fk"}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if hasLiveKey(a, "fk") {
		t.Error("orphan key still live after a failed approval")
	}
	recs, _ := s.st.AllKeys()
	for _, r := range recs {
		if r.Name == "fk" && !r.Revoked {
			t.Error("orphan key row left active in the store")
		}
	}
}

func TestAdmin_PatchKeyRestoresStoreWhenLiveKeyVanishes(t *testing.T) {
	s, a, fs := newFlakyKeyServer(t)
	if rec := keyReq(t, s, http.MethodPost, "/admin/keys", `{"name":"k1","rateLimit":5}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	// Simulate the live key disappearing (for example a config reload)
	// between the store write and the live patch.
	fs.afterUpsert = func() { a.RevokeKey("k1") }
	rec := keyReq(t, s, http.MethodPatch, "/admin/keys/k1", `{"rate_limit":99}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	fs.afterUpsert = nil
	recs, _ := s.st.AllKeys()
	if len(recs) != 1 || recs[0].RateLimit != 5 {
		t.Errorf("stored record = %+v, want the previous rate limit 5 restored", recs)
	}
}
