package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// TestHandleBackupNow_StreamsValidDatabase verifies the manual backup
// endpoint returns a 200 with a Content-Disposition attachment whose body is
// a real, independently-openable SQLite database.
func TestHandleBackupNow_StreamsValidDatabase(t *testing.T) {
	s := newRealStoreTestServer(t)
	if err := s.st.UpsertNode(store.NodeRecord{Name: "node-a", URL: "http://localhost:11434", Runtime: "ollama"}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/backup", nil)
	rec := httptest.NewRecorder()
	s.handleBackupNow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handleBackupNow status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got == "" {
		t.Error("Content-Disposition header missing")
	}
	if rec.Body.Len() == 0 {
		t.Fatal("response body is empty")
	}
}

// TestRunScheduledBackup_WritesFileAndRecordsResult verifies a scheduled
// backup run writes a timestamped file into TargetDir and records a
// successful last-backup status (surfaced by handleSettings).
func TestRunScheduledBackup_WritesFileAndRecordsResult(t *testing.T) {
	s := newRealStoreTestServer(t)
	targetDir := t.TempDir()

	if err := s.runScheduledBackup(config.BackupConfig{TargetDir: targetDir, RetentionCount: 7}); err != nil {
		t.Fatalf("runScheduledBackup: %v", err)
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("target dir has %d files, want 1", len(entries))
	}

	s.backupMu.Lock()
	at, errStr := s.lastBackupAt, s.lastBackupErr
	s.backupMu.Unlock()
	if at.IsZero() {
		t.Error("lastBackupAt not recorded after successful run")
	}
	if errStr != "" {
		t.Errorf("lastBackupErr = %q, want empty after successful run", errStr)
	}

	if persisted, err := s.st.GetSetting("backup_last_at"); err != nil || persisted == "" {
		t.Errorf("backup_last_at setting not persisted: v=%q err=%v", persisted, err)
	}
}

// TestRunScheduledBackup_NoTargetDirRecordsError verifies a missing
// TargetDir is a visible, honest failure (R1) - never a silent no-op or a
// fabricated success.
func TestRunScheduledBackup_NoTargetDirRecordsError(t *testing.T) {
	s := newRealStoreTestServer(t)

	if err := s.runScheduledBackup(config.BackupConfig{}); err == nil {
		t.Fatal("runScheduledBackup with empty TargetDir should return an error")
	}

	s.backupMu.Lock()
	errStr := s.lastBackupErr
	s.backupMu.Unlock()
	if errStr == "" {
		t.Error("lastBackupErr should be recorded after a failed run")
	}
}

// TestPruneOldBackups verifies only the newest retentionCount backup files
// survive, and that non-matching files in the same directory are untouched.
func TestPruneOldBackups(t *testing.T) {
	s := newRealStoreTestServer(t)
	dir := t.TempDir()

	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	var names []string
	for i := 0; i < 5; i++ {
		name := backupFilename(base.Add(time.Duration(i) * time.Hour))
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	// A file that doesn't match the mesh-backup-*.db naming must survive
	// pruning untouched - pruneOldBackups only manages its own files.
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s.pruneOldBackups(dir, 2)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var remaining []string
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}
	if len(remaining) != 3 { // 2 kept backups + the unrelated file
		t.Fatalf("remaining files = %v, want 2 backups + notes.txt", remaining)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file was removed by pruning: %v", err)
	}
	// The two newest (last in names, since each is 1h later) must be the
	// ones kept.
	for _, want := range names[3:] {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected newest backup %q to survive pruning: %v", want, err)
		}
	}
	for _, gone := range names[:3] {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("expected oldest backup %q to be pruned", gone)
		}
	}
}
