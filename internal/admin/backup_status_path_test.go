package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/Anirudhx7/marbor/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStatusFileRefusesSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, backupStatusFileName)

	// A real status file is read.
	if err := os.WriteFile(statusPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := readStatusFile(dir); !r.ok {
		t.Fatalf("regular status file not read: %+v", r)
	}

	// An oversized file is refused, not parsed.
	if err := os.WriteFile(statusPath, []byte(`{"x":"`+strings.Repeat("a", maxStatusFileBytes)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := readStatusFile(dir); r.ok || r.note == "" {
		t.Fatalf("oversized status file accepted: %+v", r)
	}

	// A symlink in its place is refused, even when its target is valid JSON.
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statusPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, statusPath); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if r := readStatusFile(dir); r.ok || r.note == "" {
		t.Fatalf("status file symlink was followed: %+v", r)
	}
}

func TestUpdateSettings_RejectsUnsafeBackupTargetDir(t *testing.T) {
	s := newRealStoreTestServer(t)
	good := filepath.Join(t.TempDir(), "backups")

	put := func(dir string) int {
		t.Helper()
		var cfg config.Config
		cfg.Backup.TargetDir = dir
		body, _ := json.Marshal(cfg)
		rec := httptest.NewRecorder()
		s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body)))
		return rec.Code
	}

	if code := put(good); code != http.StatusOK {
		t.Fatalf("a clean absolute backup directory was refused: status %d", code)
	}
	bad := []string{
		"relative/backups",
		good + string(filepath.Separator) + ".." + string(filepath.Separator) + "etc",
		good + "\x00/../x",
		good + "\nx",
		string(filepath.Separator),
	}
	for _, dir := range bad {
		if code := put(dir); code != http.StatusBadRequest {
			t.Errorf("target_dir %q: status %d, want 400", dir, code)
		}
		if got := s.currentTargetDir(); got != good {
			t.Fatalf("target_dir %q changed the live backup directory to %q", dir, got)
		}
	}
}
