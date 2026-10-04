package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestWireExpiryAuditWritesSystemAuditRow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "marbor.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mw := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	mw.AddKey(config.KeyConfig{Name: "bad-key", Key: "sk-secret-xyz", RateLimit: 10, ExpiresAt: "next tuesday"})
	wireExpiryAudit(mw, st)

	rows, err := st.QuerySystemAuditLog(50)
	if err != nil {
		t.Fatalf("QuerySystemAuditLog: %v", err)
	}
	var n int
	for _, r := range rows {
		if r.Action != auth.ExpiryAuditAction {
			continue
		}
		n++
		if r.Target != "bad-key" || r.Username != "system" {
			t.Errorf("row = %+v, want target bad-key by system", r)
		}
		if strings.Contains(r.Details, "sk-secret-xyz") || strings.Contains(r.Details, "next tuesday") {
			t.Errorf("details leaked a value: %q", r.Details)
		}
	}
	if n != 1 {
		t.Fatalf("got %d %s rows, want 1 (rows: %+v)", n, auth.ExpiryAuditAction, rows)
	}
}
