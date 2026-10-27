package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// ---- test doubles ------------------------------------------------------------------

// blockingSnapshotStore takes no snapshot: it says it started, then waits for the
// snapshot's context to end, the way VACUUM INTO does when it is interrupted.
type blockingSnapshotStore struct {
	store.Store
	started chan struct{}
}

func (b blockingSnapshotStore) BackupTo(ctx context.Context, _ string) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func goodBackupAt(t *testing.T, dir string, age time.Duration) string {
	t.Helper()
	name := backupFilename(time.Now().Add(-age))
	writeGz(t, dir, name, validSQLiteBytes(t))
	return name
}

// marginOn is the safety margin the server will demand on dir's volume, so a test
// can fake a free-space figure relative to it whatever disk it runs on.
func marginOn(t *testing.T, s *Server, dir string) uint64 {
	t.Helper()
	total, err := backupTotalBytes(dir)
	if err != nil {
		total = 0
	}
	db, _, err := s.st.Footprint()
	if err != nil {
		t.Fatal(err)
	}
	return backupMargin(uint64(max(db, 0)), total)
}

func newServerWithDir(t *testing.T) (*Server, string) {
	t.Helper()
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	return s, dir
}

// ---- verification ------------------------------------------------------------------

func TestVerifyLimit_FailsClosedWhenTheDatabaseSizeIsUnknown(t *testing.T) {
	base, _ := newBackupTestServer(t)
	s := NewServer(base.router, nil, config.Config{}, footprintFailStore{base.st})
	if _, err := s.verifyLimit(1 << 20); err == nil {
		t.Fatal("verifyLimit returned a bound without knowing the database size")
	}
	dir := t.TempDir()
	path := writeGz(t, dir, backupFilename(time.Now()), validSQLiteBytes(t))
	if _, err := s.verifyBackupFile(context.Background(), path); err == nil {
		t.Fatal("a compressed backup was read without a bound")
	}
}

func TestVerifyBackupFile_NameAndContentMustAgree(t *testing.T) {
	s, dir := newServerWithDir(t)
	// Plain SQLite bytes under a .db.gz name, and a gzip under a .db name.
	plainAsGz := filepath.Join(dir, backupFilename(time.Now().Add(-time.Hour)))
	if err := os.WriteFile(plainAsGz, validSQLiteBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	gzAsPlain := writeGz(t, dir, legacyBackupFilename(time.Now().Add(-2*time.Hour)), validSQLiteBytes(t))
	for _, p := range []string{plainAsGz, gzAsPlain} {
		e, err := s.verifyBackupFile(context.Background(), p)
		if err != nil || e.reason != reasonName {
			t.Errorf("%s: entry %+v err %v, want reason %q", filepath.Base(p), e, err, reasonName)
		}
		if got := s.integrityOf(p, filepath.Base(p)); got != integrityInvalid {
			t.Errorf("%s: list label %q, want invalid", filepath.Base(p), got)
		}
	}
}

func TestVerifyBackupFile_OnlyFinalNamesAreCached(t *testing.T) {
	s, dir := newServerWithDir(t)
	tmp := writeGz(t, dir, "marbor-backup-write-1.tmp", validSQLiteBytes(t))
	if _, err := s.verifyBackupFileOpts(context.Background(), tmp, true); err != nil {
		t.Fatal(err)
	}
	final := writeGz(t, dir, backupFilename(time.Now()), validSQLiteBytes(t))
	if _, err := s.verifyBackupFileOpts(context.Background(), final, false); err != nil {
		t.Fatal(err)
	}
	s.backupVerified.mu.Lock()
	n := len(s.backupVerified.m)
	s.backupVerified.mu.Unlock()
	if n != 0 {
		t.Fatalf("cache holds %d entries after a temporary-name read and an uncached read, want none", n)
	}
	if _, err := s.verifyBackupFile(context.Background(), final); err != nil {
		t.Fatal(err)
	}
	s.backupVerified.mu.Lock()
	_, ok := s.backupVerified.m[final]
	s.backupVerified.mu.Unlock()
	if !ok {
		t.Fatal("a kept backup was not cached")
	}
}

func TestIntegrityOf_RefusesALinkInTheBackupDirectory(t *testing.T) {
	s, dir := newServerWithDir(t)
	target := filepath.Join(t.TempDir(), "elsewhere.db.gz")
	writeGz(t, filepath.Dir(target), filepath.Base(target), validSQLiteBytes(t))
	link := filepath.Join(dir, backupFilename(time.Now()))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if got := s.integrityOf(link, filepath.Base(link)); got != integrityInvalid {
		t.Fatalf("a symlinked backup was labelled %q, want invalid", got)
	}
}

func TestScheduledBackup_CachesOnlyTheFinalName(t *testing.T) {
	s, dir := newServerWithDir(t)
	if err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 7}); err != nil {
		t.Fatal(err)
	}
	s.backupVerified.mu.Lock()
	defer s.backupVerified.mu.Unlock()
	if len(s.backupVerified.m) != 1 {
		t.Fatalf("cache holds %d entries, want exactly the committed file", len(s.backupVerified.m))
	}
	for p := range s.backupVerified.m {
		if !backupFilenameRE.MatchString(filepath.Base(p)) {
			t.Errorf("cached a non-final name %q", p)
		}
	}
}

// ---- space and reservations ---------------------------------------------------------

func TestGrowingHold_RefusesWhenTheVolumeCannotGrantMore(t *testing.T) {
	s, dir := newServerWithDir(t)
	margin := marginOn(t, s, dir)
	withFreeBytes(t, margin+512<<10) // 512 KiB beyond the margin
	hold, err := s.newGrowingHold(dir, 256<<10, 1<<30, "the test was running")
	if err != nil {
		t.Fatalf("initial reservation: %v", err)
	}
	defer hold.release()
	if err := hold.ensure(100 << 10); err != nil {
		t.Fatalf("ensure within the hold: %v", err)
	}
	err = hold.ensure(8 << 20)
	var refused *backupRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("ensure beyond what the volume can grant = %v, want a refusal", err)
	}
}

func TestGrowingHold_GrowsInStepsWhenThereIsRoom(t *testing.T) {
	s, dir := newServerWithDir(t)
	withFreeBytes(t, 4<<30)
	hold, err := s.newGrowingHold(dir, 1<<20, 1<<30, "the test was running")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.release()
	if err := hold.ensure(10 << 20); err != nil {
		t.Fatal(err)
	}
	s.backupSpace.mu.Lock()
	got := hold.hold.bytes
	s.backupSpace.mu.Unlock()
	if got < 10<<20+growStep {
		t.Fatalf("hold is %d bytes after ensure(10MiB), want a step ahead", got)
	}
}

func TestBackupPreflight_DefersWhenOnlyOtherWritersAreInTheWay(t *testing.T) {
	s, dir := newServerWithDir(t)
	withFreeBytes(t, marginOn(t, s, dir)+200<<20)
	s.backupSpace.mu.Lock()
	other := s.backupSpace.addLocked(dir, 150<<20)
	s.backupSpace.mu.Unlock()
	_, err := s.backupPreflight(dir, dir)
	if !errors.Is(err, errBackupDeferred) {
		t.Fatalf("preflight with another writer holding the room = %v, want a deferral", err)
	}
	other.release()
	ticket, err := s.backupPreflight(dir, dir)
	if err != nil {
		t.Fatalf("preflight with the room free: %v", err)
	}
	ticket.release()

	withFreeBytes(t, marginOn(t, s, dir)+10<<20)
	_, err = s.backupPreflight(dir, dir)
	var refused *backupRefusedError
	if !errors.As(err, &refused) || errors.Is(err, errBackupDeferred) {
		t.Fatalf("preflight on a full disk = %v, want a refusal and no deferral", err)
	}
}

func TestSpaceHold_ExpiresWhenNeverReleased(t *testing.T) {
	var l spaceLedger
	l.mu.Lock()
	h := l.addLocked("x", 100)
	h.expires = time.Now().Add(-time.Second)
	got := l.reservedOnLocked(map[string]bool{}, nil)
	_, still := l.holds[h]
	l.mu.Unlock()
	if got != 0 || still {
		t.Fatalf("an expired hold still counts: reserved=%d present=%v", got, still)
	}
}

// ---- upload rate guard ---------------------------------------------------------------

type clockReader struct {
	clk   *time.Time
	per   time.Duration // network time each read takes
	chunk int
	left  int
}

func (c *clockReader) Read(p []byte) (int, error) {
	*c.clk = c.clk.Add(c.per)
	if c.left <= 0 {
		return 0, io.EOF
	}
	k := min(len(p), c.chunk, c.left)
	c.left -= k
	return k, nil
}

func TestRateGuard_ChargesOnlyNetworkTime(t *testing.T) {
	clk := time.Unix(1000, 0)
	now := func() time.Time { return clk }
	// The network is instant; between reads the "disk" takes 5 s each time. A
	// wall-clock guard would call that too slow.
	r := &clockReader{clk: &clk, per: 0, chunk: 100, left: 1000}
	g := &rateGuard{r: r, now: now, minRate: 1000, window: 10 * time.Second, limit: 20 * time.Second}
	buf := make([]byte, 100)
	for i := 0; i < 10; i++ {
		if _, err := g.Read(buf); err != nil {
			t.Fatalf("read %d: %v (disk time must not count against the client)", i, err)
		}
		clk = clk.Add(5 * time.Second)
	}
}

func TestRateGuard_CutsASlowNetwork(t *testing.T) {
	clk := time.Unix(1000, 0)
	now := func() time.Time { return clk }
	r := &clockReader{clk: &clk, per: time.Second, chunk: 100, left: 100000}
	g := &rateGuard{r: r, now: now, minRate: 1000, window: 10 * time.Second, limit: time.Hour}
	buf := make([]byte, 100)
	var err error
	for i := 0; i < 50 && err == nil; i++ {
		_, err = g.Read(buf)
	}
	if !errors.Is(err, errUploadTooSlow) {
		t.Fatalf("100 bytes per second against a 1000 per second minimum ended with %v, want errUploadTooSlow", err)
	}
}

func TestRateGuard_TotalAllowanceIsCappedAndCountsNetworkTime(t *testing.T) {
	if got := uploadDeadline(maxUploadedBackupSize * 100); got != uploadMaxDeadline {
		t.Fatalf("deadline for an enormous upload = %v, want the cap %v", got, uploadMaxDeadline)
	}
	clk := time.Unix(1000, 0)
	r := &clockReader{clk: &clk, per: 3 * time.Second, chunk: 1 << 20, left: 1 << 30}
	g := &rateGuard{r: r, now: func() time.Time { return clk }, minRate: 1, window: time.Hour, limit: 10 * time.Second}
	buf := make([]byte, 1<<20)
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		_, err = g.Read(buf)
	}
	if !errors.Is(err, errUploadTooSlow) {
		t.Fatalf("exceeding the total allowance ended with %v", err)
	}
}

func TestRespondUploadReadError_RefusalIs507(t *testing.T) {
	rec := httptest.NewRecorder()
	respondUploadReadError(rec, httptest.NewRequest(http.MethodPost, "/x", nil), &backupRefusedError{msg: "the disk filled up while the upload was being saved"})
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status %d, want 507", rec.Code)
	}
}

// ---- committing ----------------------------------------------------------------------

func TestCommitBackupFile_WorksWithoutHardLinks(t *testing.T) {
	s, dir := newServerWithDir(t)
	old := linkFile
	linkFile = func(string, string) error { return errors.New("hard links are not supported here") }
	t.Cleanup(func() { linkFile = old })
	var names []string
	for i := 0; i < 2; i++ {
		tmp := filepath.Join(dir, "marbor-backup-write-"+string(rune('1'+i))+".tmp")
		if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		name, err := s.commitBackupFile(dir, tmp, ".db.gz", false)
		if err != nil || name == "" {
			t.Fatalf("commit %d: %q %v", i, name, err)
		}
		if _, serr := os.Stat(tmp); !os.IsNotExist(serr) {
			t.Errorf("temporary file %d survived the rename", i)
		}
		names = append(names, name)
	}
	if names[0] == names[1] {
		t.Fatalf("two commits in the same second share the name %q: the second replaced the first", names[0])
	}
}

func TestCommitWarning_IsAFixedSentence(t *testing.T) {
	got := commitWarning(newBackupFailure("the backup was saved but the directory could not be flushed to disk", errors.New(`flush C:\secret\path: denied`)))
	if !strings.Contains(got, "could not be flushed") || strings.Contains(got, "secret") {
		t.Fatalf("warning %q leaks detail or lost its meaning", got)
	}
	if other := commitWarning(errors.New(`open /srv/private: denied`)); strings.Contains(other, "private") {
		t.Fatalf("unclassified warning leaks detail: %q", other)
	}
}

// ---- retention ------------------------------------------------------------------------

func TestPrune_APoolWithinTheLimitIsNotRead(t *testing.T) {
	s, dir := newServerWithDir(t)
	for i := 0; i < 2; i++ {
		p := filepath.Join(dir, backupFilename(time.Now().Add(-time.Duration(i+1)*time.Hour)))
		if err := os.WriteFile(p, []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.pruneBackups(context.Background(), dir, 7, false); got != "" {
		t.Fatalf("pruneBackups = %q, want nothing to report", got)
	}
	s.backupVerified.mu.Lock()
	n := len(s.backupVerified.m)
	s.backupVerified.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d files were read although the pool was within its limit", n)
	}
}

func TestPrune_DeletesOnlyVerifiedExcessAndPointsAtDelete(t *testing.T) {
	s, dir := newServerWithDir(t)
	newest := goodBackupAt(t, dir, time.Hour)
	older := goodBackupAt(t, dir, 2*time.Hour)
	damaged := backupFilename(time.Now().Add(-3 * time.Hour))
	if err := os.WriteFile(filepath.Join(dir, damaged), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.pruneBackups(context.Background(), dir, 1, false)
	if !strings.Contains(got, "marbor backup delete") || !strings.Contains(got, "damaged") {
		t.Fatalf("pruneBackups = %q, want it to name the damaged backup and how to delete it", got)
	}
	files := strings.Join(backupFilesIn(t, dir), ",")
	if !strings.Contains(files, newest) || !strings.Contains(files, damaged) || strings.Contains(files, older) {
		t.Fatalf("kept %s: want the newest and the damaged one kept, the older good one deleted", files)
	}
}

func TestDeleteExcess_LeavesAFileThatChangedAfterVerification(t *testing.T) {
	s, dir := newServerWithDir(t)
	name := goodBackupAt(t, dir, time.Hour)
	path := filepath.Join(dir, name)
	id, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced with something else entirely"), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed := s.deleteExcess(dir, []deletion{{name: name, id: id}}); failed != 0 {
		t.Fatalf("%d failures", failed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a file that changed after it was verified was deleted anyway")
	}
}

func TestRunPrune_QueuesExactlyOneFurtherPass(t *testing.T) {
	s, dir := newServerWithDir(t)
	passes := 0
	prunePassDone = func() {
		passes++
		if passes == 1 {
			// A request arrives while the pass is running, twice: they merge.
			s.runPrune(context.Background(), dir, 5, false)
			s.runPrune(context.Background(), dir, 5, false)
		}
	}
	t.Cleanup(func() { prunePassDone = nil })
	s.runPrune(context.Background(), dir, 5, false)
	if passes != 2 {
		t.Fatalf("%d passes, want the running one plus one queued", passes)
	}
	s.backupMu.Lock()
	running, pending := s.pruneRunning[0], s.prunePending[0].pending
	s.backupMu.Unlock()
	if running || pending {
		t.Fatalf("pool state after the passes: running=%v pending=%v", running, pending)
	}
}

// ---- deleting a backup -----------------------------------------------------------------

func TestHandleDeleteBackup(t *testing.T) {
	s, dir := newServerWithDir(t)
	name := goodBackupAt(t, dir, time.Hour)
	del := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleDeleteBackup(rec, httptest.NewRequest(http.MethodDelete, "/admin/backup?name="+q, nil))
		return rec
	}
	for _, bad := range []string{"", "..%2Fx", "notes.txt", "marbor-backup-1.db"} {
		if rec := del(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: %d, want 400", bad, rec.Code)
		}
	}
	if rec := del(backupFilename(time.Now().Add(-48 * time.Hour))); rec.Code != http.StatusNotFound {
		t.Errorf("missing file: %d, want 404", rec.Code)
	}
	unpin := s.pinBackup(name)
	if rec := del(name); rec.Code != http.StatusConflict {
		t.Errorf("a backup in use: %d, want 409", rec.Code)
	}
	unpin()
	s.housekeeping.Store(true)
	if rec := del(name); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("during housekeeping: %d retry-after %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	s.housekeeping.Store(false)
	if rec := del(name); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if len(backupFilesIn(t, dir)) != 0 {
		t.Fatal("the backup is still there")
	}
}

func TestHandleDeleteBackup_NeedsADirectory(t *testing.T) {
	s, _ := newBackupTestServer(t)
	rec := httptest.NewRecorder()
	s.handleDeleteBackup(rec, httptest.NewRequest(http.MethodDelete, "/admin/backup?name="+backupFilename(time.Now()), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d, want 400", rec.Code)
	}
}

// ---- housekeeping, deferral and shutdown --------------------------------------------------

func TestHandlers_AreRefusedWithAReasonWhileHousekeepingRuns(t *testing.T) {
	s, dir := newServerWithDir(t)
	ch := make(chan string, 1)
	s.SetRestoreChannel(ch)
	name := goodBackupAt(t, dir, time.Hour)
	s.housekeeping.Store(true)
	cases := map[string]func(*httptest.ResponseRecorder){
		"backup now": func(rec *httptest.ResponseRecorder) {
			s.handleBackupNow(rec, httptest.NewRequest(http.MethodPost, "/admin/backup", nil))
		},
		"upload": func(rec *httptest.ResponseRecorder) {
			s.handleUploadBackup(rec, httptest.NewRequest(http.MethodPost, "/admin/backup/upload", nil))
		},
		"restore": func(rec *httptest.ResponseRecorder) {
			s.handleRestoreBackup(rec, httptest.NewRequest(http.MethodPost, "/admin/backup/restore", strings.NewReader(`{"filename":"`+name+`"}`)))
		},
	}
	for what, call := range cases {
		rec := httptest.NewRecorder()
		call(rec)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "leftover") {
			t.Errorf("%s: %d %q retry-after %q, want 503, a reason and Retry-After", what, rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
		}
	}
}

func TestManualBackup_TooManyAtOnceIs429WithRetryAfter(t *testing.T) {
	s, _ := newServerWithDir(t)
	for i := 0; i < maxManualBackups; i++ {
		if !s.acquireManualSlot() {
			t.Fatalf("slot %d refused", i)
		}
	}
	rec := httptest.NewRecorder()
	s.handleBackupNow(rec, httptest.NewRequest(http.MethodPost, "/admin/backup", nil))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "at most") {
		t.Fatalf("%d %q retry-after %q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
}

func TestScheduledBackup_IsDeferredNotFailedWhileTheDiskIsBusy(t *testing.T) {
	for _, busy := range []string{"housekeeping", "heavy"} {
		t.Run(busy, func(t *testing.T) {
			s, dir := newServerWithDir(t)
			cfg := config.BackupConfig{TargetDir: dir, RetentionCount: 7}
			if busy == "housekeeping" {
				s.housekeeping.Store(true)
			} else {
				s.heavyRunning.Store(true)
			}
			err := s.runScheduledBackup(context.Background(), cfg)
			if !errors.Is(err, errBackupDeferred) {
				t.Fatalf("run = %v, want a deferral", err)
			}
			snap := s.backupStatusSnapshot(dir)
			if snap.Scheduled.Result != "" || !strings.Contains(snap.Scheduled.Note, "deferred") {
				t.Fatalf("scheduled record %+v, want no result and a deferred note", snap.Scheduled)
			}
			_, attempt, result := s.scheduleState(dir)
			if !attempt.IsZero() || result != "" {
				t.Fatalf("a deferral left an attempt (%v, %q): it would start the failure backoff", attempt, result)
			}
			if !scheduledBackupDue(time.Now(), time.Time{}, attempt, result, 24) {
				t.Fatal("a deferred run is not due again at once")
			}
			s.housekeeping.Store(false)
			s.heavyRunning.Store(false)
			if err := s.runScheduledBackup(context.Background(), cfg); err != nil {
				t.Fatalf("the retry failed: %v", err)
			}
			if snap := s.backupStatusSnapshot(dir); snap.Scheduled.Note != "" || snap.Scheduled.Result != "success" {
				t.Fatalf("after the retry the record is %+v", snap.Scheduled)
			}
		})
	}
}

func TestStopBackups_CancelsARunningScheduledBackupAndRecordsWhy(t *testing.T) {
	base, _ := newBackupTestServer(t)
	started := make(chan struct{}, 1)
	s := NewServer(base.router, nil, config.Config{}, blockingSnapshotStore{Store: base.st, started: started})
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.mu.Lock()
	s.schedCtx, s.schedCancel = ctx, cancel
	s.mu.Unlock()

	result := make(chan error, 1)
	s.goBackground("test backup", func(bg context.Context) {
		result <- s.runScheduledBackup(bg, config.BackupConfig{TargetDir: dir, RetentionCount: 7})
	})
	<-started
	s.StopBackups()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v, want cancellation", err)
	}
	snap := s.backupStatusSnapshot(dir)
	if snap.Scheduled.Result != "cancelled" || !strings.Contains(snap.Scheduled.Error, "shutting down") {
		t.Fatalf("scheduled record %+v, want a cancellation that says why", snap.Scheduled)
	}
	for _, n := range dirNames(t, dir) {
		if n != backupStatusFileName {
			t.Errorf("an interrupted run left %q", n)
		}
	}
	// Nothing new starts once backups are stopping.
	ran := false
	s.goBackground("late", func(context.Context) { ran = true })
	if ran {
		t.Fatal("background work started after StopBackups")
	}
}

func TestCancelledOutcomeText(t *testing.T) {
	s, dir := newServerWithDir(t)
	s.recordBackupOutcome(dir, backupSourceManual, context.Canceled)
	if got := s.backupStatusSnapshot(dir).Manual.Error; !strings.Contains(got, "requester went away") {
		t.Errorf("manual cancel text %q", got)
	}
	s.recordBackupOutcome(dir, backupSourceScheduled, context.Canceled)
	if got := s.backupStatusSnapshot(dir).Scheduled.Error; strings.Contains(got, "shutting down") {
		t.Errorf("scheduled cancel while running says shutdown: %q", got)
	}
	s.backupsStopping.Store(true)
	s.recordBackupOutcome(dir, backupSourceManual, context.Canceled)
	if got := s.backupStatusSnapshot(dir).Manual.Error; !strings.Contains(got, "shutting down") {
		t.Errorf("manual cancel during shutdown text %q", got)
	}
}

func TestStartBackupScheduler_IsIdempotentAndStopsCleanly(t *testing.T) {
	s, _ := newBackupTestServer(t)
	ctx := context.Background()
	s.StartBackupScheduler(ctx)
	s.StartBackupScheduler(ctx) // a second call is a no-op, not a second goroutine
	s.StopBackups()
	if s.housekeeping.Load() {
		t.Fatal("the housekeeping flag stayed set after the scheduler stopped")
	}
	s.StopBackups() // and stopping twice is fine
}

func TestRecoverLog_SwallowsAPanic(t *testing.T) {
	func() {
		defer recoverLog("test")
		panic("boom")
	}()
}

func TestTrySweep_ReleasesEveryLock(t *testing.T) {
	s, dir := newServerWithDir(t)
	if !s.trySweep(dir) {
		t.Fatal("sweep did not run on an idle server")
	}
	if s.housekeeping.Load() || !s.backupRunMu.TryLock() || !s.backupHeavyMu.TryLock() {
		t.Fatal("sweep left a flag or lock held")
	}
	s.backupRunMu.Unlock()
	s.backupHeavyMu.Unlock()
	// A busy run lock means skip and retry, not block.
	s.backupRunMu.Lock()
	if s.trySweep(dir) {
		t.Fatal("sweep ran while a backup held the run lock")
	}
	if s.housekeeping.Load() {
		t.Fatal("a skipped sweep left the housekeeping flag set")
	}
	s.backupRunMu.Unlock()
}

func TestFSTimeout_GivesUpOnAHungCall(t *testing.T) {
	old := housekeepingScanTimeout
	housekeepingScanTimeout = 20 * time.Millisecond
	t.Cleanup(func() { housekeepingScanTimeout = old })
	release := make(chan struct{})
	err := fsTimeout(func() error { <-release; return nil })
	close(release)
	if !errors.Is(err, errFSTimeout) {
		t.Fatalf("fsTimeout = %v, want errFSTimeout", err)
	}
}

func TestReadDirTimeout_ConcurrentCallsShareOneListing(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries, err := readDirTimeout(dir)
			if err != nil || len(entries) != 2 {
				t.Errorf("readDirTimeout = %d entries, %v", len(entries), err)
			}
		}()
	}
	wg.Wait()
	scanMu.Lock()
	left := len(scans)
	scanMu.Unlock()
	if left != 0 {
		t.Fatalf("%d finished listings were not cleaned up", left)
	}
}

func TestCleanupBackupLeftovers_ReportsAnUnreadableDirectorySeparately(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stuck, unreadable := cleanupBackupLeftovers(file); stuck != 0 || !unreadable {
		t.Skipf("this platform reports a file listed as a directory as (%d, %v)", stuck, unreadable)
	}
	if stuck, unreadable := cleanupBackupLeftovers(filepath.Join(t.TempDir(), "missing")); stuck != 0 || unreadable {
		t.Fatalf("a missing directory is neither stuck files nor unreadable: (%d, %v)", stuck, unreadable)
	}
	s, dir := newServerWithDir(t)
	s.recordLeftovers(dir, 0, true)
	if got := s.backupStatusSnapshot(dir); got.DirScanError == "" || got.LeftoverFiles != 0 {
		t.Fatalf("status %+v, want a scan error and no leftover count", got)
	}
}

// ---- status ------------------------------------------------------------------------------

func TestStatus_RunningFollowsTheFlagNotALockProbe(t *testing.T) {
	s, dir := newServerWithDir(t)
	s.backupRunMu.Lock() // held, but nothing is being backed up
	if s.backupStatusSnapshot(dir).Running {
		t.Error("a held lock alone reads as running")
	}
	s.backupRunMu.Unlock()
	s.backupRunning.Store(true)
	if !s.backupStatusSnapshot(dir).Running {
		t.Error("the running flag was ignored")
	}
}

func TestStatusFiles_AreReadWithoutHoldingTheStatusLock(t *testing.T) {
	s, dir := newServerWithDir(t)
	s.backupMu.Lock()
	done := make(chan struct{})
	go func() {
		s.readStatusFor(dir)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.backupMu.Unlock()
		t.Fatal("reading the status files waited for backupMu")
	}
	s.backupMu.Unlock()
}

func TestScratchLeftovers_AreCountedPerServer(t *testing.T) {
	s, dir := newServerWithDir(t)
	other, _ := newBackupTestServer(t)
	stuck := filepath.Join(dir, "marbor-backup-scratch-1.db")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.removeScratch(stuck) // a non-empty directory cannot be removed as a file
	if s.scratchLeftovers.Load() != 1 || other.scratchLeftovers.Load() != 0 {
		t.Fatalf("counts s=%d other=%d, want 1 and 0", s.scratchLeftovers.Load(), other.scratchLeftovers.Load())
	}
	if got := s.backupStatusSnapshot(dir).LeftoverFiles; got != 1 {
		t.Fatalf("status leftover count %d, want 1", got)
	}
}

func TestDeadlineObserver_IsPerServer(t *testing.T) {
	s1, _ := newBackupTestServer(t)
	s2, _ := newBackupTestServer(t)
	var mu sync.Mutex
	seen := 0
	s1.SetDeadlineObserver(func(string, error) { mu.Lock(); seen++; mu.Unlock() })
	s2.newDeadlineControl(httptest.NewRecorder()).clearRead()
	if seen != 0 {
		t.Fatal("another server's transfer reached this observer")
	}
	s1.newDeadlineControl(httptest.NewRecorder()).clearRead()
	if seen != 1 {
		t.Fatalf("own transfer observed %d times, want 1", seen)
	}
}

func TestNoStore_SetsTheHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	noStore(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestCountingWriter_RemembersTheStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: rec}
	cw.WriteHeader(http.StatusPartialContent)
	_, _ = cw.Write([]byte("abc"))
	if cw.status != http.StatusPartialContent || cw.n != 3 {
		t.Fatalf("status %d bytes %d", cw.status, cw.n)
	}
}

func TestDownload_RangeRequestIsServedAsPartContent(t *testing.T) {
	s, dir := newServerWithDir(t)
	name := goodBackupAt(t, dir, time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/admin/backup/download?name="+name, nil)
	req.Header.Set("Range", "bytes=0-9")
	rec := httptest.NewRecorder()
	s.handleDownloadBackup(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 10 {
		t.Fatalf("%d with %d bytes, want 206 with 10", rec.Code, rec.Body.Len())
	}
}

// ---- restore state -------------------------------------------------------------------------

func TestReadPendingRestore_DropsUntrustedFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "live.db")
	write := func(p restorePending) {
		data, _ := json.Marshal(p)
		if err := os.WriteFile(restorePendingPath(dbPath), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(restorePending{Ref: "../../etc/passwd", File: "a\nb\x01c", Staged: "../evil"})
	got, ok := readPendingRestore(dbPath)
	if !ok || got.Ref != "" || got.Staged != "" || got.File != "abc" {
		t.Fatalf("marker read back as %+v", got)
	}
	for _, bad := range []string{"other.restoring-0123456789abcdef", "sub/live.db.restoring-0123456789abcdef", `sub\live.db.restoring-0123456789abcdef`, "live.db"} {
		write(restorePending{Ref: "0123456789abcdef", Staged: bad})
		if got, _ := readPendingRestore(dbPath); got.Staged != "" {
			t.Errorf("staged name %q was trusted", bad)
		}
	}
	good := stagedRestoreName(dbPath, "0123456789abcdef")
	write(restorePending{Ref: "0123456789abcdef", Staged: good})
	if got, _ := readPendingRestore(dbPath); got.Staged != good || got.Ref != "0123456789abcdef" {
		t.Errorf("a well-formed marker was rejected: %+v", got)
	}
}

func TestSanitizeOutcome(t *testing.T) {
	o := restoreOutcome{Ref: "not-a-ref", Result: "weird", Error: "bad\x1b[31mred\n" + strings.Repeat("x", 1000), Note: "ok"}
	sanitizeOutcome(&o)
	if o.Ref != "" || o.Result != backupResultFailed || strings.ContainsAny(o.Error, "\x1b\n") || len(o.Error) > maxMarkerText {
		t.Fatalf("outcome not sanitized: %+v", o)
	}
}

func TestHandOffRestore_ClosedSlotLeavesNoMarkerAndIdentityIsTracked(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	ch := make(chan string, 1)
	s.SetRestoreChannel(ch)
	ref := "0123456789abcdef"
	staged := stagedRestorePath(dbPath, ref)
	if err := os.WriteFile(staged, validSQLiteBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}

	// Shutdown began while the restore was being staged.
	if err := s.beginRestore(); err != nil {
		t.Fatal(err)
	}
	s.CloseRestores()
	if err := s.handOffRestore(staged, ref, "x.db.gz"); !errors.Is(err, errRestoreClosed) {
		t.Fatalf("handOff after close = %v", err)
	}
	if _, err := os.Stat(restorePendingPath(dbPath)); !os.IsNotExist(err) {
		t.Fatal("a refused hand-off left a pending marker")
	}

	// A normal hand-off records the identity of what was validated.
	s2, dbPath2 := newBackupTestServer(t)
	ch2 := make(chan string, 1)
	s2.SetRestoreChannel(ch2)
	staged2 := stagedRestorePath(dbPath2, ref)
	if err := os.WriteFile(staged2, validSQLiteBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s2.beginRestore(); err != nil {
		t.Fatal(err)
	}
	if err := s2.handOffRestore(staged2, ref, "x.db.gz"); err != nil {
		t.Fatal(err)
	}
	if got := <-ch2; got != staged2 {
		t.Fatalf("handed off %q", got)
	}
	if !s2.StagedRestoreUnchanged(staged2) {
		t.Fatal("an untouched staged file was not recognised")
	}
	if s2.StagedRestoreUnchanged(staged2 + "x") {
		t.Fatal("another path was recognised")
	}
	if err := os.WriteFile(staged2, append(validSQLiteBytes(t), 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if s2.StagedRestoreUnchanged(staged2) {
		t.Fatal("a staged file that changed size was recognised as unchanged")
	}
}

func TestRestoreNotCarriedOut_IsReportedPrecisely(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	ref := "0123456789abcdef"
	data, _ := json.Marshal(restorePending{Ref: ref, Staged: stagedRestoreName(dbPath, ref), At: time.Now()})
	if err := os.WriteFile(restorePendingPath(dbPath), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The staged file is gone and nothing says why: only "unknown" can be said.
	p, _ := readPendingRestore(dbPath)
	if got := s.synthesizePendingOutcome(dbPath, p).Error; !strings.Contains(got, "check the data") {
		t.Fatalf("unknown case text %q", got)
	}
	MarkRestoreNotCarriedOut(dbPath)
	p, _ = readPendingRestore(dbPath)
	if !p.NotCarriedOut {
		t.Fatal("the marker did not record that the restore was abandoned")
	}
	if got := s.synthesizePendingOutcome(dbPath, p).Error; !strings.Contains(got, "was not carried out") {
		t.Fatalf("abandoned case text %q", got)
	}
}

func TestAcknowledgeRestoreResult_FailedRemovalKeepsTheResultShown(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	s.backupMu.Lock()
	s.lastRestore = &restoreOutcome{Result: backupResultFailed, Error: "boom"}
	s.backupMu.Unlock()
	// A non-empty directory where the saved result belongs cannot be removed.
	if err := os.Mkdir(restoreStatusPath(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restoreStatusPath(dbPath), "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeRestoreResult("-"); err == nil {
		t.Fatal("acknowledgement reported success although the saved copy stayed")
	}
	s.backupMu.Lock()
	still := s.lastRestore != nil
	s.backupMu.Unlock()
	if !still {
		t.Fatal("the result was cleared from memory although the saved copy could not be removed")
	}
	if err := s.AcknowledgeRestoreResult("0123456789abcdef"); !errors.Is(err, errRestoreRefMismatch) {
		t.Fatalf("a different ref = %v, want a mismatch", err)
	}
}

func TestCleanupStagedRestores_RemovesOnlyThisDatabasesStagedFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "live.db")
	for _, n := range []string{"live.db.restoring-0123456789abcdef", "live.db.restoring-0123456789abcdef-wal", "live.db", "other.db.restoring-1"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if stuck := cleanupStagedRestores(dbPath); stuck != 0 {
		t.Fatalf("%d stuck", stuck)
	}
	if got := strings.Join(dirNames(t, dir), ","); got != "live.db,other.db.restoring-1" {
		t.Fatalf("left %s", got)
	}
}

// ---- additions for clients that wait for a restart or judge the schedule ----

// /health carries an identifier of this run of the process, so a client can tell
// a restarted marbor from the old one still shutting down.
func TestHealth_CarriesTheProcessStartTime(t *testing.T) {
	s, _ := newServerWithDir(t)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body struct {
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, body.StartedAt); err != nil {
		t.Fatalf("started_at = %q: %v", body.StartedAt, err)
	}
}

// The status says what the scheduler is set to do and since when a backup has
// been running.
func TestBackupStatus_ReportsTheScheduleAndTheRunStart(t *testing.T) {
	s, dir := newServerWithDir(t)
	s.mu.Lock()
	s.cfg.Backup.Enabled = true
	s.cfg.Backup.IntervalHours = 6
	s.mu.Unlock()
	info := s.backupStatusSnapshot(dir)
	if !info.ScheduleEnabled || info.ScheduleIntervalHours != 6 || info.RunningSince != "" {
		t.Fatalf("idle: %+v", info)
	}
	if !s.tryRun() {
		t.Fatal("could not claim the run")
	}
	info = s.backupStatusSnapshot(dir)
	s.endRun()
	if !info.Running || info.RunningSince == "" {
		t.Fatalf("running: %+v", info)
	}
	if again := s.backupStatusSnapshot(dir); again.Running || again.RunningSince != "" {
		t.Fatalf("after the run: %+v", again)
	}
}
