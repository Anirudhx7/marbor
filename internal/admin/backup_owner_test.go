package admin

import (
	"strings"
	"testing"
)

func TestForeignOwnerWarning(t *testing.T) {
	cases := []struct {
		name        string
		owner, proc uint32
		warn        bool
	}{
		{"owned by the service user", 1000, 1000, false},
		{"owned by root, service user is not root", 0, 1000, false},
		{"owned by root, service is root", 0, 0, false},
		{"owned by another user, service is root", 1000, 0, true},
		{"owned by another user, service is not root", 1001, 1000, true},
	}
	for _, c := range cases {
		got := foreignOwnerWarning(c.owner, c.proc)
		if (got != "") != c.warn {
			t.Errorf("%s: foreignOwnerWarning(%d, %d) = %q, want warning=%v", c.name, c.owner, c.proc, got, c.warn)
		}
		if got != "" && (strings.Contains(got, "/") || strings.Contains(got, "\\")) {
			t.Errorf("%s: the warning must not contain a path: %q", c.name, got)
		}
	}
}

func TestBackupDirStateDoesNotWarnAboutADirectoryTheServiceOwns(t *testing.T) {
	dir := t.TempDir()
	if _, warning := backupDirState(dir); strings.Contains(warning, "owned by another user") {
		t.Fatalf("a directory created by this process was reported as foreign: %q", warning)
	}
}
