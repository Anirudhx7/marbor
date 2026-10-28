//go:build !windows

package admin

import (
	"os"
	"strings"
	"testing"
)

// Reporting another owner needs a directory that another user owns, which only
// root can arrange, so this test runs where the suite runs as root (a container,
// WSL) and skips elsewhere. The decision itself is covered by
// TestForeignOwnerWarning.
func TestBackupDirStateWarnsAboutADirectoryOwnedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to hand a directory to another user")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 54321, 54321); err != nil {
		t.Skipf("cannot chown here: %v", err)
	}
	exists, warning := backupDirState(dir)
	if !exists || !strings.Contains(warning, "owned by another user") {
		t.Fatalf("backupDirState = %v, %q; want an ownership warning", exists, warning)
	}
	if strings.Contains(warning, dir) {
		t.Fatalf("the warning contains the directory path: %q", warning)
	}
}
