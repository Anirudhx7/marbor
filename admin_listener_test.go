package main

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func newPendingAdminServer(t *testing.T) *admin.Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "marbor.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return admin.NewServerWithBootstrap(r, nil, config.Config{}, st, admin.BootstrapOptions{
		DataDir: dir,
		Getenv:  func(string) string { return "" },
	})
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestServeAdmin_RefusesWildcardBindWhileInitialPasswordActive(t *testing.T) {
	// Arrange
	s := newPendingAdminServer(t)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: time.Second}
	done := make(chan struct{})

	// Act
	go func() {
		defer close(done)
		serveAdmin(srv, s, "0.0.0.0:0")
	}()

	// Assert: serveAdmin returns on its own (nothing is being served) so the
	// rest of the process is free to keep running.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = srv.Close()
		t.Fatal("serveAdmin kept serving a non-loopback bind with an initial password active")
	}
}

func TestServeAdmin_ServesLoopbackBind(t *testing.T) {
	// Arrange
	s := newPendingAdminServer(t)
	addr := freeLoopbackAddr(t)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: time.Second}
	go serveAdmin(srv, s, addr)
	t.Cleanup(func() { _ = srv.Close() })

	// Act
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = http.Get("http://" + addr + "/health")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Assert
	if err != nil {
		t.Fatalf("loopback admin listener never came up: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
}

func TestApplyAdminBindEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		wantBind string
		wantRet  string
	}{
		{"unset keeps the stored bind", "", ":8080", ""},
		{"override wins", "127.0.0.1:8080", "127.0.0.1:8080", "127.0.0.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MARBOR_ADMIN_BIND_ADDRESS", tt.env)
			cfg := &config.Config{}
			cfg.Admin.BindAddress = ":8080"

			got, stored := applyAdminBindEnv(cfg)

			if got != tt.wantRet || cfg.Admin.BindAddress != tt.wantBind {
				t.Errorf("returned %q, bind %q; want %q, %q", got, cfg.Admin.BindAddress, tt.wantRet, tt.wantBind)
			}
			if stored != ":8080" {
				t.Errorf("stored bind returned as %q, want the pre-override :8080", stored)
			}
		})
	}
}
