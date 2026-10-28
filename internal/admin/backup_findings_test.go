package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// footprintFailStore reports a failing Footprint, as when the database file or
// its log cannot be examined.
type footprintFailStore struct{ store.Store }

func (footprintFailStore) Footprint() (int64, int64, error) {
	return 0, 0, errors.New("stat /secret/volume/live.db: boom")
}

func newServerWithStore(t *testing.T, st store.Store) *Server {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{}, st)
}

func listBody(t *testing.T, s *Server) (backups []backupFileInfo, status backupStatusInfo, raw string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleListBackups(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/list", nil))
	var resp struct {
		Backups []backupFileInfo `json:"backups"`
		Status  backupStatusInfo `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("list body: %v: %s", err, rec.Body.String())
	}
	return resp.Backups, resp.Status, rec.Body.String()
}

func TestFootprintFailureFailsTheRunAndWritesNothing(t *testing.T) {
	_, dbPath := newBackupTestServer(t)
	st, err := store.Open(dbPath + ".2")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newServerWithStore(t, footprintFailStore{st})
	dir := t.TempDir()

	err = s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 3})
	if err == nil {
		t.Fatal("a backup that cannot size the database must fail, not run unchecked")
	}
	if len(backupFilesIn(t, dir)) != 0 {
		t.Errorf("files written despite the failed size check: %v", dirNames(t, dir))
	}
	info := s.backupStatusSnapshot(dir)
	if info.Scheduled.Result != backupResultFailed || !strings.Contains(info.Scheduled.Error, "database size") {
		t.Errorf("status = %+v, want a failure that names the database size", info.Scheduled)
	}
	if strings.Contains(info.Scheduled.Error, "/secret") {
		t.Errorf("status error leaks a path: %q", info.Scheduled.Error)
	}
}

func TestDecompressLimitRefusesWhenFreeSpaceCannotBeRead(t *testing.T) {
	s, _ := newBackupTestServer(t)
	old := backupFreeBytes
	backupFreeBytes = func(string) (uint64, error) { return 0, errors.New("statfs /secret/mount: boom") }
	t.Cleanup(func() { backupFreeBytes = old })

	limit, _, err := s.decompressLimit(t.TempDir(), 1<<20)
	if err == nil || limit != 0 {
		t.Fatalf("decompressLimit = %d, %v; want a refusal", limit, err)
	}
	msg := err.Error()
	if strings.Contains(msg, "limit 0") || strings.Contains(msg, "/secret") {
		t.Errorf("message %q must not say 'limit 0' or carry a path", msg)
	}
}

func TestScheduledAndManualOutcomesAreSeparate(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	if err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 3}); err != nil {
		t.Fatal(err)
	}
	s.recordBackupOutcome(dir, backupSourceManual, fmt.Errorf("open /var/secret/x: boom"))

	info := s.backupStatusSnapshot(dir)
	if info.Scheduled.Result != backupResultSuccess || info.Scheduled.SuccessAt == "" {
		t.Errorf("scheduled = %+v, want its own success", info.Scheduled)
	}
	if info.Manual.Result != backupResultFailed {
		t.Errorf("manual = %+v, want failed", info.Manual)
	}
	if strings.Contains(info.Manual.Error, "/var/secret") {
		t.Errorf("manual error leaks a path: %q", info.Manual.Error)
	}
	if info.LastSource != backupSourceManual || info.LastSuccessAt == "" {
		t.Errorf("last source %q, last success %q: want manual attempt with the scheduled success time kept", info.LastSource, info.LastSuccessAt)
	}
	_, _, lastResult := s.scheduleState(dir)
	if lastResult != backupResultSuccess {
		t.Errorf("a failed manual download must not back off the scheduler (scheduled result %q)", lastResult)
	}
}

func TestPublicBackupErrorKeywordsAndCancel(t *testing.T) {
	if r, _ := publicBackupError(context.Canceled); r != backupResultCancelled {
		t.Errorf("cancel -> %q, want cancelled", r)
	}
	if r, _ := publicBackupError(&backupRefusedError{msg: "no room"}); r != backupResultRefused {
		t.Errorf("refusal -> %q, want refused", r)
	}
	r, text := publicBackupError(errors.New("rename /srv/data/a /srv/data/b: denied"))
	if r != backupResultFailed || strings.Contains(text, "/srv") || !strings.Contains(text, "ref ") {
		t.Errorf("unknown error -> %q %q, want a generic failure with a reference and no path", r, text)
	}
}

func TestStatusReloadsWhenTheDirectoryChanges(t *testing.T) {
	s, _ := newBackupTestServer(t)
	a, b := t.TempDir(), t.TempDir()
	s.recordBackupOutcome(a, backupSourceScheduled, errors.New("boom"))
	if got := s.backupStatusSnapshot(b); got.LastResult != "" {
		t.Errorf("another directory shows %q, want no history", got.LastResult)
	}
	if got := s.backupStatusSnapshot(a); got.LastResult != backupResultFailed {
		t.Errorf("original directory shows %q, want its failure back", got.LastResult)
	}
}

func TestStatusThatCannotBeSavedSaysSo(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	// A directory where the status file must go makes the save fail.
	if err := os.Mkdir(filepath.Join(dir, backupStatusFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	s.recordBackupOutcome(dir, backupSourceScheduled, nil)
	if got := s.backupStatusSnapshot(dir); got.StatusNotSaved == "" {
		t.Error("an unsaved status must be reported as not saved")
	}
	if err := os.Remove(filepath.Join(dir, backupStatusFileName)); err != nil {
		t.Fatal(err)
	}
	s.recordBackupOutcome(dir, backupSourceScheduled, nil)
	if got := s.backupStatusSnapshot(dir); got.StatusNotSaved != "" {
		t.Errorf("after a successful save the note must clear, got %q", got.StatusNotSaved)
	}
}

func TestDedupIgnoresADamagedCandidateAndKeepsTheNewBackup(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	cfg := config.BackupConfig{TargetDir: dir, RetentionCount: 9}
	if err := s.runScheduledBackup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	first := backupFilesIn(t, dir)
	if len(first) != 1 {
		t.Fatalf("setup: %v", first)
	}
	// Damage the middle of the kept file but leave its tail (the recorded
	// fingerprint) intact, then change its mtime so no cached check applies.
	path := filepath.Join(dir, first[0])
	data, _ := os.ReadFile(path)
	for i := 40; i < 60 && i < len(data)-120; i++ {
		data[i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if err := s.runScheduledBackup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(backupFilesIn(t, dir)); n != 2 {
		t.Fatalf("%d backups, want 2: a damaged file must not stand in for a good copy", n)
	}
}

func TestPruneCountsOnlyGoodBackupsAndKeepsDamagedOnes(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	valid := validSQLiteBytes(t)
	ts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	older := backupFilename(ts)
	newer := backupFilename(ts.Add(time.Hour))
	damaged := backupFilename(ts.Add(2 * time.Hour))
	writeGz(t, dir, older, valid)
	writeGz(t, dir, newer, valid)
	full, _ := os.ReadFile(filepath.Join(dir, newer))
	if err := os.WriteFile(filepath.Join(dir, damaged), full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	// The damaged file is kept and reported, never silently counted.
	if problem := s.pruneBackups(context.Background(), dir, 1, false); !strings.Contains(problem, "damaged") {
		t.Fatalf("prune must report the damaged backup, got %q", problem)
	}
	got := backupFilesIn(t, dir)
	if len(got) != 2 || got[0] != newer || got[1] != damaged {
		t.Fatalf("after prune: %v, want the newest good file and the damaged one (damaged files are flagged, never deleted or counted)", got)
	}

	backups, _, _ := listBody(t, s)
	_ = backups
	setBackupTargetDir(s, dir)
	backups, _, _ = listBody(t, s)
	integrity := map[string]string{}
	for _, b := range backups {
		integrity[b.Name] = b.Integrity
	}
	if integrity[damaged] != integrityInvalid {
		t.Errorf("damaged file integrity = %q, want invalid", integrity[damaged])
	}
	if integrity[newer] == integrityInvalid || integrity[newer] == "" {
		t.Errorf("good file integrity = %q, want ok or unverified", integrity[newer])
	}
}

func TestPrunePinnedBackupIsNotDeleted(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	valid := validSQLiteBytes(t)
	ts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	oldest, mid, newest := backupFilename(ts), backupFilename(ts.Add(time.Hour)), backupFilename(ts.Add(2*time.Hour))
	for _, n := range []string{oldest, mid, newest} {
		writeGz(t, dir, n, valid)
	}
	release := s.pinBackup(oldest)
	s.pruneBackups(context.Background(), dir, 1, false)
	release()
	got := backupFilesIn(t, dir)
	if len(got) != 2 || got[0] != oldest || got[1] != newest {
		t.Fatalf("after prune: %v, want the pinned oldest and the newest", got)
	}
}

func TestCommitBackupFileNeverOverwritesAndGivesUniqueNames(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	names := map[string]bool{}
	for i := 0; i < 4; i++ {
		tmp := filepath.Join(dir, fmt.Sprintf("marbor-backup-write-%d.tmp", i))
		if err := os.WriteFile(tmp, []byte(fmt.Sprintf("content-%d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		name, err := s.commitBackupFile(dir, tmp, ".db.gz", false)
		if err != nil || name == "" {
			t.Fatalf("commit %d: %q, %v", i, name, err)
		}
		if names[name] {
			t.Fatalf("name %s handed out twice", name)
		}
		names[name] = true
		got, _ := os.ReadFile(filepath.Join(dir, name))
		if string(got) != fmt.Sprintf("content-%d", i) {
			t.Fatalf("%s holds %q: an earlier backup was overwritten or mixed up", name, got)
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Errorf("temporary file %d still present", i)
		}
	}
}

func TestVerifyArchiveRejectsAMismatchedFingerprint(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	tmp, fp, raw, err := s.writeBackupArchive(context.Background(), dir, dir, "marbor-backup-write-*.tmp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.verifyArchive(context.Background(), tmp, fp, raw); err != nil {
		t.Fatalf("a faithful archive failed verification: %v", err)
	}
	wrong := strings.Repeat("ab", 32)
	if err := s.verifyArchive(context.Background(), tmp, wrong, raw); err == nil {
		t.Fatal("an archive that does not match the snapshot must not verify")
	}
	if err := s.verifyArchive(context.Background(), tmp, fp, raw+1); err == nil {
		t.Fatal("a size mismatch must not verify")
	}
}

func TestUploadByteBudgetIsEnforcedWithUnknownLength(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	// Room for the margin plus one kibibyte.
	withFreeBytes(t, backupMinMargin+1024)
	oldTotal := backupTotalBytes
	backupTotalBytes = func(string) (uint64, error) { return 0, nil }
	t.Cleanup(func() { backupTotalBytes = oldTotal })

	req := newUploadRequest(t, "big.db", bytes.Repeat([]byte("x"), 64<<10))
	req.ContentLength = -1 // a client that does not say how much it will send
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-budget upload = %d %s, want 507", rec.Code, rec.Body.String())
	}
	for _, n := range dirNames(t, dir) {
		if strings.HasSuffix(n, ".tmp") {
			t.Errorf("rejected upload left %q", n)
		}
	}
}

func TestUploadTooLargeIs413NotAServerError(t *testing.T) {
	s, _ := newBackupTestServer(t)
	setBackupTargetDir(s, t.TempDir())
	old := maxUploadedBackupSize
	maxUploadedBackupSize = 4096
	t.Cleanup(func() { maxUploadedBackupSize = old })
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, newUploadRequest(t, "big.db", bytes.Repeat([]byte("x"), 64<<10)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d %s, want 413", rec.Code, rec.Body.String())
	}
}

func TestUploadDiskFullMapsTo507AndNoPathLeaks(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	withFreeBytes(t, 1<<20) // below the margin
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, newUploadRequest(t, "a.db", validSQLiteBytes(t)))
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload with no room = %d, want 507", rec.Code)
	}
	if strings.Contains(rec.Body.String(), dir) {
		t.Errorf("response leaks the backup directory: %s", rec.Body.String())
	}
}

func TestUploadDoesNotPruneScheduledBackups(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	cfg := config.BackupConfig{TargetDir: dir, RetentionCount: 1}
	s.mu.Lock()
	s.cfg.Backup = cfg
	s.mu.Unlock()
	valid := validSQLiteBytes(t)
	ts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s1, s2 := backupFilename(ts), backupFilename(ts.Add(time.Hour))
	writeGz(t, dir, s1, valid)
	writeGz(t, dir, s2, valid)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(valid)
	_ = zw.Close()
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, newUploadRequest(t, "u.db.gz", gz.Bytes()))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	got := backupFilesIn(t, dir)
	hasS1, hasS2 := false, false
	for _, n := range got {
		hasS1 = hasS1 || n == s1
		hasS2 = hasS2 || n == s2
	}
	// The upload's content equals a kept backup, so it may reuse that entry; in
	// that case no new file exists, but no scheduled backup may be lost either.
	if !hasS1 || !hasS2 {
		t.Fatalf("scheduled backups were pruned by an upload: %v (response %v)", got, m)
	}
}

func TestUploadsArePrunedAmongThemselvesOnly(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	valid := validSQLiteBytes(t)
	ts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	sched := backupFilename(ts)
	u1 := backupBaseName(ts.Add(time.Hour)) + "-upload.db.gz"
	u2 := backupBaseName(ts.Add(2*time.Hour)) + "-upload.db.gz"
	for _, n := range []string{sched, u1, u2} {
		writeGz(t, dir, n, valid)
	}
	if problem := s.pruneBackups(context.Background(), dir, 1, true); problem != "" {
		t.Fatal(problem)
	}
	got := backupFilesIn(t, dir)
	if len(got) != 2 || got[0] != sched || got[1] != u2 {
		t.Fatalf("after pruning uploads: %v, want the scheduled backup and only the newest upload", got)
	}
}

func TestUploadedFileGetsUploadNameAndOwnRetention(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	var gz bytes.Buffer
	if _, _, err := store.CompressBackup(bytes.NewReader(validSQLiteBytes(t)), &gz); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, newUploadRequest(t, "mine.db.gz", gz.Bytes()))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	files := backupFilesIn(t, dir)
	if len(files) != 1 || !strings.Contains(files[0], "-upload.db.gz") {
		t.Fatalf("files = %v, want one -upload.db.gz", files)
	}
	st, _ := os.Stat(filepath.Join(dir, files[0]))
	if st.Size() != int64(gz.Len()) {
		t.Errorf("stored %d bytes for a %d byte upload that already carried its fingerprint: the tail must not be appended twice", st.Size(), gz.Len())
	}
	backups, _, _ := listBody(t, s)
	if len(backups) != 1 || !backups[0].Uploaded {
		t.Errorf("list = %+v, want the file marked uploaded", backups)
	}
}

func TestUploadedForgedFingerprintIsNotTrusted(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	valid := validSQLiteBytes(t)
	realFP, _, err := store.CompressBackup(bytes.NewReader(valid), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(valid)
	_ = zw.Close()
	forged := strings.Repeat("cd", 32)
	if err := store.AppendFingerprintTail(&gz, forged); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, newUploadRequest(t, "forged.db.gz", gz.Bytes()))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	files := backupFilesIn(t, dir)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	got, ok := store.ReadFingerprintTail(filepath.Join(dir, files[0]))
	if !ok || got != realFP {
		t.Fatalf("stored tail = %q, %v; want the fingerprint this process computed (%s), not the uploaded one", got, ok, realFP)
	}
}

func TestHeavyOperationsAreSingleFlight(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	s.SetRestoreChannel(make(chan string, 1))
	name := backupFilename(time.Now())
	writeGz(t, dir, name, validSQLiteBytes(t))

	s.backupHeavyMu.Lock()
	defer s.backupHeavyMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleUploadBackup(rec, newUploadRequest(t, "a.db", validSQLiteBytes(t)))
	if rec.Code != http.StatusConflict {
		t.Errorf("upload while another check runs = %d, want 409", rec.Code)
	}
	if rec := restoreRequest(s, name); rec.Code != http.StatusConflict {
		t.Errorf("restore while another check runs = %d, want 409", rec.Code)
	}
}

func TestRestoreWithoutRoomIsRefusedBeforeTheRestart(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ch := make(chan string, 1)
	s.SetRestoreChannel(ch)
	name := backupFilename(time.Now())
	writeGz(t, dir, name, validSQLiteBytes(t))
	withFreeBytes(t, 1<<20)

	rec := restoreRequest(s, name)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("restore with no room on the staging volume = %d %s, want 507", rec.Code, rec.Body.String())
	}
	if len(ch) != 0 {
		t.Fatal("the restart must not be triggered when staging cannot fit")
	}
	if left := stagedFilesNextTo(t, dbPath); len(left) != 0 {
		t.Errorf("a refused restore left a staged file: %v", left)
	}
	if strings.Contains(rec.Body.String(), filepath.Dir(dbPath)) || strings.Contains(rec.Body.String(), dir) {
		t.Errorf("response leaks a path: %s", rec.Body.String())
	}
}

func TestServedErrorsAndStatusCarryNoFilesystemPaths(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	s.SetRestoreChannel(make(chan string, 1))
	ts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	trunc := backupFilename(ts)
	good := writeGz(t, dir, backupFilename(ts.Add(time.Hour)), validSQLiteBytes(t))
	data, _ := os.ReadFile(good)
	_ = os.WriteFile(filepath.Join(dir, trunc), data[:len(data)/2], 0o600)

	rec := restoreRequest(s, trunc)
	for _, secret := range []string{dir, filepath.Dir(dbPath), os.TempDir()} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("restore error leaks %q: %s", secret, rec.Body.String())
		}
	}
	s.recordBackupOutcome(dir, backupSourceManual, fmt.Errorf("create %s: denied", filepath.Join(dir, "x.tmp")))
	_, status, raw := listBody(t, s)
	if strings.Contains(raw, dir) {
		t.Errorf("list response leaks the backup directory: %s", raw)
	}
	if strings.Contains(status.DirWarning, dir) {
		t.Errorf("dir_warning leaks the directory: %q", status.DirWarning)
	}
}

func TestDownloadHeadersAreNoStoreAndNosniff(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	name := backupFilename(time.Now())
	writeGz(t, dir, name, validSQLiteBytes(t))
	rec := httptest.NewRecorder()
	s.handleDownloadBackup(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/download?name="+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v, want no-store and nosniff", rec.Header())
	}
}

func TestBackupNowSetsContentLengthSoTruncationIsDetectable(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	seedRequestLog(t, dbPath, 200)
	rec := httptest.NewRecorder()
	s.handleBackupNow(rec, httptest.NewRequest(http.MethodPost, "/admin/backup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup now = %d %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Length"), fmt.Sprint(rec.Body.Len()); got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestHousekeepingClearsLeftoversAndAbsorbsTheRestoreOutcome(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	dir := t.TempDir()
	staged := dbPath + ".restoring-0123456789abcdef"
	stagedSidecar := staged + "-wal"
	scratch := filepath.Join(dir, "marbor-backup-scratch-9.db")
	keep := filepath.Join(dir, backupFilename(time.Now()))
	for _, p := range []string{staged, stagedSidecar, scratch, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteRestoreOutcome(dbPath, errors.New("rename /srv/x /srv/y: denied"), ""); err != nil {
		t.Fatal(err)
	}

	s.runBackupHousekeeping(context.Background(), dir)

	for _, p := range []string{staged, stagedSidecar, scratch} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived start-up housekeeping", filepath.Base(p))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("housekeeping removed a real backup")
	}
	info := s.backupStatusSnapshot(dir)
	if info.LastRestore == nil || info.LastRestore.Result != backupResultFailed {
		t.Fatalf("last restore = %+v, want the failure the previous process recorded", info.LastRestore)
	}
	if strings.Contains(info.LastRestore.Error, "/srv") {
		t.Errorf("restore error leaks a path: %q", info.LastRestore.Error)
	}
	if _, err := os.Stat(restoreOutcomePath(dbPath)); !os.IsNotExist(err) {
		t.Error("the restore outcome marker must be consumed once absorbed")
	}
}

func TestBackupMarginAndWalHeadroom(t *testing.T) {
	const mib, gib = uint64(1 << 20), uint64(1 << 30)
	cases := []struct{ db, vol, want uint64 }{
		{10 * mib, 0, 128 * mib},        // floor
		{10 * gib, 0, gib / 2},          // 5 percent of the database
		{mib, 100 * gib, gib},           // 1 percent of the volume
		{mib, 100000 * gib, 4 * gib},    // volume share is capped
		{10 * gib, 100 * gib, gib},      // volume share beats 5% only when larger (1 GiB vs 0.5 GiB)
		{100 * gib, 100 * gib, 5 * gib}, // 5 percent of a huge database
		{0, 0, 128 * mib},
	}
	for _, c := range cases {
		if got := backupMargin(c.db, c.vol); got != c.want {
			t.Errorf("backupMargin(%d, %d) = %d, want %d", c.db, c.vol, got, c.want)
		}
	}
	if got := walHeadroom(10*mib, 0); got != walHeadroomMin {
		t.Errorf("small database headroom = %d, want the floor %d", got, walHeadroomMin)
	}
	if got := walHeadroom(10*gib, 0); got != gib {
		t.Errorf("10 GiB headroom = %d, want 10 percent (%d)", got, gib)
	}
	if got := walHeadroom(mib, 900*mib); got != 900*mib {
		t.Errorf("a log already larger than the estimate must be planned for as it stands, got %d", got)
	}
}

func TestOpenBackupFileRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, backupFilename(time.Now()))
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	f, _, err := openBackupFile(p)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, errNotRegularBackup) {
		t.Fatalf("openBackupFile(dir) = %v, want errNotRegularBackup", err)
	}
}

// stagedFilesNextTo lists the staged restore files (and sidecars) beside dbPath.
func stagedFilesNextTo(t *testing.T, dbPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if isStagedRestoreName(e.Name(), filepath.Base(dbPath)) {
			out = append(out, e.Name())
		}
	}
	return out
}
