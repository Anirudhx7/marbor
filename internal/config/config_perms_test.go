package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSaveConfigPerms verifies SaveConfig writes the config file as 0600
// (owner read/write only) so the plaintext API keys, admin token, and
// cloud-provider keys are not world-readable.
func TestSaveConfigPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := SaveConfig(path, Config{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected new config mode 0600, got %o", perm)
	}
}

// TestSaveConfigReNarrowsExistingFile proves the re-widening bug is fixed:
// when the file already exists at 0644 (as every dashboard "Save Settings"
// rewrite would leave it without an explicit chmod), SaveConfig must narrow
// it back to 0600.
func TestSaveConfigReNarrowsExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Pre-create the file world-readable, simulating an older deployment.
	if err := os.WriteFile(path, []byte("proxy:\n  port: 11434\n"), 0644); err != nil {
		t.Fatalf("pre-create config: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat pre-created config: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0644 {
		t.Fatalf("setup precondition failed: expected 0644, got %o", perm)
	}

	if err := SaveConfig(path, Config{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected re-narrowed config mode 0600, got %o", perm)
	}
}
