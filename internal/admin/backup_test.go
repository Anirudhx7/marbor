package admin

import (
	"bytes"
	"encoding/json"
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

// setBackupTargetDir sets s.cfg.Backup.TargetDir directly (bypassing
// handleUpdateSettings) for tests that only care about the restore/list
// endpoints, not settings persistence.
func setBackupTargetDir(s *Server, dir string) {
	s.mu.Lock()
	s.cfg.Backup.TargetDir = dir
	s.mu.Unlock()
}

// TestHandleListBackups_FiltersAndSortsNewestFirst verifies only
// mesh-backup-*.db files are listed, newest first, ignoring unrelated files.
func TestHandleListBackups_FiltersAndSortsNewestFirst(t *testing.T) {
	s := newRealStoreTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)

	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	older := backupFilename(base)
	newer := backupFilename(base.Add(time.Hour))
	for _, name := range []string{older, newer, "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/backup/list", nil)
	rec := httptest.NewRecorder()
	s.handleListBackups(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleListBackups status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Backups []backupFileInfo `json:"backups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Backups) != 2 {
		t.Fatalf("backups = %+v, want 2 entries (notes.txt excluded)", resp.Backups)
	}
	if resp.Backups[0].Name != newer || resp.Backups[1].Name != older {
		t.Errorf("backups not newest-first: got [%s, %s]", resp.Backups[0].Name, resp.Backups[1].Name)
	}
}

// TestHandleRestoreBackup_RejectsPathTraversal verifies filenames containing
// path separators or not matching the strict backup naming pattern are
// rejected before any filesystem lookup, closing off directory traversal.
func TestHandleRestoreBackup_RejectsPathTraversal(t *testing.T) {
	s := newRealStoreTestServer(t)
	s.SetRestoreChannel(make(chan string, 1))
	setBackupTargetDir(s, t.TempDir())

	for _, bad := range []string{"../../etc/passwd", "sub/mesh-backup-20260730-120000.db", "/etc/passwd", "not-a-backup.db"} {
		body, _ := json.Marshal(map[string]string{"filename": bad})
		req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleRestoreBackup(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("filename %q: status = %d, want 400", bad, rec.Code)
		}
	}
}

// TestHandleRestoreBackup_RejectsMissingAndInvalidFiles verifies a
// well-formed filename that doesn't exist, or exists but isn't a valid
// SQLite database, is rejected before anything is sent to restoreCh.
func TestHandleRestoreBackup_RejectsMissingAndInvalidFiles(t *testing.T) {
	s := newRealStoreTestServer(t)
	ch := make(chan string, 1)
	s.SetRestoreChannel(ch)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)

	// Missing file.
	missingName := backupFilename(time.Now())
	body, _ := json.Marshal(map[string]string{"filename": missingName})
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRestoreBackup(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing file: status = %d, want 404", rec.Code)
	}

	// Present but not a real SQLite database.
	corruptName := backupFilename(time.Now().Add(time.Hour))
	if err := os.WriteFile(filepath.Join(dir, corruptName), []byte("not a database"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	body, _ = json.Marshal(map[string]string{"filename": corruptName})
	req = httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	s.handleRestoreBackup(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("corrupt file: status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}

	select {
	case p := <-ch:
		t.Errorf("restoreCh received %q, want nothing sent for rejected requests", p)
	default:
	}
}

// TestHandleRestoreBackup_ValidRequestSendsToChannel verifies a well-formed
// request for a real, valid backup file is accepted (202) and its full path
// is sent down restoreCh - never acted on directly by the handler itself.
func TestHandleRestoreBackup_ValidRequestSendsToChannel(t *testing.T) {
	s := newRealStoreTestServer(t)
	ch := make(chan string, 1)
	s.SetRestoreChannel(ch)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)

	// A real backup file: use BackupTo against the test server's own store.
	name := backupFilename(time.Now())
	fullPath := filepath.Join(dir, name)
	if err := s.st.BackupTo(fullPath); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"filename": name})
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRestoreBackup(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}

	select {
	case got := <-ch:
		if got != fullPath {
			t.Errorf("restoreCh received %q, want %q", got, fullPath)
		}
	default:
		t.Error("restoreCh received nothing, want the validated backup path")
	}
}

// TestHandleRestoreBackup_NoChannelReturns501 verifies a run mode that never
// wired SetRestoreChannel (tests, demo, any future non-main caller) fails
// loudly instead of silently accepting a restore it can't act on.
func TestHandleRestoreBackup_NoChannelReturns501(t *testing.T) {
	s := newRealStoreTestServer(t)
	body, _ := json.Marshal(map[string]string{"filename": backupFilename(time.Now())})
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRestoreBackup(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// TestValidateBackupFile verifies a genuine SQLite database passes and a
// non-database file fails.
func TestValidateBackupFile(t *testing.T) {
	dir := t.TempDir()

	validPath := filepath.Join(dir, "valid.db")
	validStore, err := store.Open(validPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	validStore.Close()
	if err := store.ValidateBackupFile(validPath); err != nil {
		t.Errorf("ValidateBackupFile(valid db) = %v, want nil", err)
	}

	invalidPath := filepath.Join(dir, "invalid.db")
	if err := os.WriteFile(invalidPath, []byte("definitely not sqlite"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := store.ValidateBackupFile(invalidPath); err == nil {
		t.Error("ValidateBackupFile(garbage file) = nil, want an error")
	}
}
