package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// These tests pin what the backup code does when something other than a plain
// file sits where it expects one: a symlink (including a dangling one), a
// junction or a hard link. They exist because the backup directory is shared
// with whoever can write to it, so every create, delete and replace must act
// on the entry itself and never on what a link points at.

// victimFile makes a file outside dir whose content must survive every test.
func victimFile(t *testing.T) (path, content string) {
	t.Helper()
	content = "must survive"
	path = filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func requireIntact(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("the file outside the backup directory was changed: %q, %v", got, err)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

func TestLeftoverCleanupRemovesALinkNotItsTarget(t *testing.T) {
	dir := t.TempDir()
	victim, want := victimFile(t)
	link := filepath.Join(dir, "marbor-backup-scratch-123.db")
	symlinkOrSkip(t, victim, link)

	if stuck, unreadable := cleanupBackupLeftovers(dir); stuck != 0 || unreadable {
		t.Fatalf("cleanup: stuck=%d unreadable=%v", stuck, unreadable)
	}
	requireIntact(t, victim, want)
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover link itself should have been removed: %v", err)
	}
}

func TestPruneAndDeleteNeverActOnASymlinkTarget(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	victim, want := victimFile(t)
	var names []string
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("marbor-backup-2026010%d-000000.db.gz", i+1)
		symlinkOrSkip(t, victim, filepath.Join(dir, name))
		names = append(names, name)
	}

	s.pruneBackups(context.Background(), dir, 1, false)
	requireIntact(t, victim, want)

	if err := s.deleteBackup(dir, names[0]); !errors.Is(err, errNotRegularBackup) {
		t.Fatalf("deleteBackup of a symlink = %v, want errNotRegularBackup", err)
	}
	requireIntact(t, victim, want)
	if fi, err := os.Lstat(filepath.Join(dir, names[0])); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("deleteBackup must leave a refused entry in place: %v", err)
	}
}

func TestCommitBackupFileSkipsDanglingSymlinksAndNeverWritesThroughThem(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "should-not-be-created.db.gz")

	t0 := time.Now().UTC()
	var planted []string
	for i := -1; i <= 8; i++ {
		link := filepath.Join(dir, backupBaseName(t0.Add(time.Duration(i)*time.Second))+".db.gz")
		symlinkOrSkip(t, outside, link)
		planted = append(planted, link)
	}

	tmp := filepath.Join(dir, "marbor-backup-write-1.tmp")
	if err := os.WriteFile(tmp, []byte("backup bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := s.commitBackupFile(dir, tmp, ".db.gz", false)
	if err != nil || name == "" {
		t.Fatalf("commit: %q, %v", name, err)
	}
	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("commit wrote through a planted symlink: %v", err)
	}
	for _, link := range planted {
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("a planted link was replaced: %v", err)
		}
		if filepath.Base(link) == name {
			t.Fatalf("commit reused a planted name %s", name)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the committed file is not a regular file: %v", err)
	}
}

func TestWriteFileAtomicReplacesALinkWithoutTouchingItsTarget(t *testing.T) {
	dir := t.TempDir()
	victim, want := victimFile(t)
	dest := filepath.Join(dir, backupStatusFileName)
	symlinkOrSkip(t, victim, dest)

	if _, err := writeFileAtomic(dest, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	requireIntact(t, victim, want)
	if fi, err := os.Lstat(dest); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the destination should now be a regular file: %v", err)
	}
}

func TestOpenBackupFileRefusesLinksAndJunctions(t *testing.T) {
	dir := t.TempDir()
	victim, _ := victimFile(t)

	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, victim, link)
	if _, _, err := openBackupFile(link); !errors.Is(err, errNotRegularBackup) {
		t.Errorf("symlink: %v, want errNotRegularBackup", err)
	}

	if runtime.GOOS == "windows" {
		targetDir := t.TempDir()
		junction := filepath.Join(dir, "junction")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, targetDir).CombinedOutput(); err != nil {
			t.Logf("junction unavailable: %v %s", err, out)
		} else if _, _, err := openBackupFile(junction); err == nil {
			t.Error("a directory junction was opened as a backup file")
		}
	}

	// A hard link is a second name for a regular file the process can already
	// read, so it is accepted; the file's content is still validated as a backup
	// or status file before anything is shown or restored.
	hard := filepath.Join(dir, "hard")
	if err := os.Link(victim, hard); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	f, _, err := openBackupFile(hard)
	if err != nil {
		t.Fatalf("hard link: %v", err)
	}
	f.Close()
}

func TestCleanupStagedRestoresTouchesOnlyStagedNames(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := []string{
		"marbor.db.restoring-0123456789abcdef",
		"marbor.db.restoring-0123456789abcdef-journal",
		"marbor.db.restoring-0123456789abcdef-wal",
		"marbor.db.restoring-0123456789abcdef-shm",
	}
	keep := []string{
		"marbor.db",
		"marbor.db.restoring",
		"marbor.db.restoring-notes.txt",
		"marbor.db.restoring-0123456789abcdef.bak",
		"other.db.restoring-0123456789abcdef",
	}
	for _, name := range append(append([]string{}, staged...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if stuck := cleanupStagedRestores(dbPath); stuck != 0 {
		t.Fatalf("stuck = %d", stuck)
	}
	for _, name := range staged {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("staged file %s was not removed: %v", name, err)
		}
	}
	for _, name := range keep {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("unrelated file %s was removed: %v", name, err)
		}
	}
}

func TestWriteStatusFileKeepsItsFileWhenTheDirectoryIsAnAliasOfTheDatabaseDirectory(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	dbDir := filepath.Dir(dbPath)
	alias := filepath.Join(t.TempDir(), "alias")
	symlinkOrSkip(t, dbDir, alias)

	if note := s.writeStatusFile(alias, []byte(`{}`)); note != "" {
		t.Fatalf("writeStatusFile note = %q", note)
	}
	if _, err := os.Lstat(filepath.Join(dbDir, backupStatusFileName)); err != nil {
		t.Fatalf("the status file just written was removed through the alias: %v", err)
	}
}
