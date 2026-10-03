package admin

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// syncBuffer is a goroutine-safe log sink: NewServer starts goroutines that
// may log while the test reads the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// A malformed persisted expires_at must show as expired in the key list, never
// active, matching what auth enforces.
func TestAdmin_KeyListMalformedExpiryNotActive(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{Auth: config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "bad", Key: "sk-bad", RateLimit: 1000, ExpiresAt: "garbage"},
			{Name: "ok", Key: "sk-ok", RateLimit: 1000, ExpiresAt: "2099-01-01"},
			{Name: "none", Key: "sk-none", RateLimit: 1000},
		},
	}}
	a := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, a, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/keys status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	want := map[string]string{"bad": "expired", "ok": "active", "none": "active"}
	got := map[string]string{}
	for _, k := range list {
		got[k.Name] = k.Status
	}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("key %q status = %q, want %q (all: %v)", name, got[name], status, got)
		}
	}
}

// Credentials carried over from the legacy single-admin table hold an iterated
// SHA-256 hash. Login verifies bcrypt only, so they cannot authenticate: the
// migrated account is locked out until reset, and the default admin/admin
// password does not become a way in.
func TestLegacySHA256AdminCredentialsCannotAuthenticate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	const legacyPassword = "legacy-Passw0rd!"
	// Shape of an old hex(sha256) hash; the value only has to not be bcrypt.
	legacyHash := strings.Repeat("0", 64)
	if err := st.SetAdminCreds(store.AdminCreds{Username: "admin", PasswordHash: legacyHash, Salt: "00"}); err != nil {
		t.Fatalf("SetAdminCreds: %v", err)
	}

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st) // migrates the legacy row into users

	u, err := st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("migrated user missing: %v", err)
	}
	if u.PasswordHash != legacyHash {
		t.Fatalf("migration changed the stored hash; the test no longer models a legacy credential")
	}

	for _, pw := range []string{legacyPassword, defaultAdminPassword, legacyHash} {
		body := `{"username":"admin","password":"` + pw + `"}`
		req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("login with %q against a legacy hash: status = %d, want 401", pw, rec.Code)
		}
	}
	if verifyPassword(legacyHash, legacyPassword) {
		t.Error("verifyPassword accepted a non-bcrypt legacy hash")
	}
}

// The default-credential warning fires on first creation and again on a restart
// while the password is still admin, and stops once it has been changed.
func TestDefaultAdminCredentialWarning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "warn.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)

	buf := captureLog(t)
	NewServer(r, nil, config.Config{}, st)
	for _, want := range []string{"admin / admin", "password change is required", "plaintext HTTP", "TLS"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("first boot warning missing %q: %s", want, buf.String())
		}
	}

	buf.Reset()
	NewServer(r, nil, config.Config{}, st) // restart, password unchanged
	if !strings.Contains(buf.String(), "default admin login admin / admin is still active") {
		t.Errorf("restart with default credential gave no warning: %s", buf.String())
	}

	newHash, err := hashPassword("a-Much-Better-Passw0rd")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	u, _ := st.GetUserByUsername("admin")
	u.PasswordHash = newHash
	u.MustChangePassword = false
	if err := st.UpdateUser(u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	buf.Reset()
	NewServer(r, nil, config.Config{}, st)
	if strings.Contains(buf.String(), "admin / admin") {
		t.Errorf("warning still logged after the password was changed: %s", buf.String())
	}
}
