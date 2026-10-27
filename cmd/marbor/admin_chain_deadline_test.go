package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// An on-demand backup streamed through the REAL admin middleware chain (the
// recover and security-header wrappers around the admin handler) must be able to
// set connection deadlines. A wrapper that hides the underlying connection makes
// every deadline call fail with http.ErrNotSupported, which would silently turn
// the stall protection off; this test fails if that ever happens again.
func TestBackupNow_ThroughTheRealAdminChainCanSetDeadlines(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{}
	cfg.Backup.TargetDir = t.TempDir()
	srv := admin.NewServer(router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil), nil, cfg, st)

	var mu sync.Mutex
	var calls int
	var unsupported []string
	srv.SetDeadlineObserver(func(op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if errors.Is(err, http.ErrNotSupported) {
			unsupported = append(unsupported, op)
		}
	})
	t.Cleanup(func() { srv.SetDeadlineObserver(nil) })

	ts := httptest.NewServer(adminChain(srv.Handler()))
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := client.Post(ts.URL+path, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}
	if resp := post("/admin/login", `{"username":"admin","password":"admin"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if resp := post("/admin/change-password", `{"new_password":"Fresh-Pass-1"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("change-password: %d", resp.StatusCode)
	}
	resp := post("/admin/backup", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("backup now: %d %v %q", resp.StatusCode, err, body)
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("the response is not a gzip stream (%d bytes)", len(body))
	}

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("no deadline was ever set while streaming the backup")
	}
	if len(unsupported) > 0 {
		t.Fatalf("deadline calls were not supported through the admin chain: %v", unsupported)
	}
}
