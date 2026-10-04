package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/winexit"
)

// applyAdminBindEnv applies the environment-only admin bind override, which
// takes precedence over the stored setting and is never persisted. It returns
// the forced address ("" when no override is set), the stored address it
// replaced, and exits on a malformed one.
func applyAdminBindEnv(cfg *config.Config) (override, stored string) {
	raw := os.Getenv(bootstrapcred.EnvAdminBind)
	stored = cfg.Admin.BindAddress
	applied, err := config.ApplyAdminBindOverride(cfg, raw)
	if err != nil {
		winexit.Fatalf("%s: %v", bootstrapcred.EnvAdminBind, err)
	}
	if !applied {
		return "", stored
	}
	log.Printf("Admin bind address %q is forced by %s for this run only; the stored setting is unchanged", cfg.Admin.BindAddress, bootstrapcred.EnvAdminBind)
	return cfg.Admin.BindAddress, stored
}

// dataDirOf returns the directory holding the database, which is where the
// generated first-boot password file lives. An in-memory/disabled store ("-")
// has none.
func dataDirOf(dbPath string) string {
	if dbPath == "-" || dbPath == "" {
		return ""
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return filepath.Dir(dbPath)
	}
	return filepath.Dir(abs)
}

// serveAdmin binds the admin address, lets the first-boot credential gate
// judge the listener's real bound address, and serves only if it passes. A
// refusal closes the listener and logs why; the proxy and metrics servers are
// unaffected.
func serveAdmin(srv *http.Server, adminSrv *admin.Server, bind string) {
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		log.Fatalf("Admin server error: %v", err)
	}
	if gateErr := adminSrv.AdminListenerGate(ln, bind); gateErr != nil {
		_ = ln.Close()
		log.Printf("ERROR: %v", gateErr)
		return
	}
	log.Printf("Admin dashboard listening on %s", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Admin server error: %v", err)
	}
}
