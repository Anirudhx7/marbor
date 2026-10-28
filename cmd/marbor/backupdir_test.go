package main

import (
	"path/filepath"
	"testing"
)

func TestStoredBackupDir(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "backups")
	good := filepath.Join(t.TempDir(), "custom")

	if got := storedBackupDir(good, fallback); got != good {
		t.Errorf("absolute stored dir = %q, want it kept", got)
	}
	if got := storedBackupDir("", fallback); got != "" {
		t.Errorf("empty stored dir = %q, want it kept (backups not configured)", got)
	}
	rel := storedBackupDir("backups", fallback)
	if !filepath.IsAbs(rel) {
		t.Errorf("relative stored dir resolved to %q, want an absolute path", rel)
	}
	for _, bad := range []string{"/", filepath.Join(good, "..", "..", "x") + "\x00", good + "\nx", `\.\C:\x`} {
		if got := storedBackupDir(bad, fallback); got != fallback {
			t.Errorf("storedBackupDir(%q) = %q, want the fallback", bad, got)
		}
	}
}
