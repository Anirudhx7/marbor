package admin

import (
	"bytes"
	"context"
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

func restoreServer(t *testing.T) (*Server, string, chan string, string) {
	t.Helper()
	s, dbPath := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ch := make(chan string, 4)
	s.SetRestoreChannel(ch)
	name := backupFilename(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))
	writeGz(t, dir, name, validSQLiteBytes(t))
	return s, dbPath, ch, name
}

// While a restore is handed off, a second request is refused with 409 and the
// first staged file is untouched.
func TestRestore_SecondRequestIsRefusedAndKeepsTheFirstStagedFile(t *testing.T) {
	s, _, ch, name := restoreServer(t)
	if rec := restoreRequest(s, name); rec.Code != http.StatusAccepted {
		t.Fatalf("first restore: %d %s", rec.Code, rec.Body.String())
	}
	staged := <-ch
	if rec := restoreRequest(s, name); rec.Code != http.StatusConflict {
		t.Fatalf("second restore: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("the first staged file is gone: %v", err)
	}
	if got := s.CloseRestores(); got != staged {
		t.Errorf("CloseRestores = %q, want the handed-off %q", got, staged)
	}
}

// After shutdown began, no restore is accepted (503) and nothing is staged.
func TestRestore_RefusedAfterShutdownBegan(t *testing.T) {
	s, dbPath, ch, name := restoreServer(t)
	if got := s.CloseRestores(); got != "" {
		t.Fatalf("CloseRestores with nothing pending = %q", got)
	}
	rec := restoreRequest(s, name)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("restore during shutdown: %d %s, want 503", rec.Code, rec.Body.String())
	}
	if len(ch) != 0 {
		t.Error("a refused restore reached the channel")
	}
	if left := stagedFilesNextTo(t, dbPath); len(left) != 0 {
		t.Errorf("a refused restore left staged files: %v", left)
	}
}

// The pending marker is durable before the 202, and a restore that never
// recorded an outcome shows up as "did not complete" at the next start.
func TestRestore_PendingMarkerBecomesAnOutcomeAtNextStart(t *testing.T) {
	s, dbPath, ch, name := restoreServer(t)
	if rec := restoreRequest(s, name); rec.Code != http.StatusAccepted {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	staged := <-ch
	p, ok := readPendingRestore(dbPath)
	if !ok || p.Ref == "" || p.Staged != filepath.Base(staged) {
		t.Fatalf("pending marker = %+v, %v", p, ok)
	}

	// A fresh server over the same database: the previous process died.
	s2, _ := newBackupTestServer(t)
	s2.st = s.st
	s2.absorbRestoreOutcome()
	s2.backupMu.Lock()
	got := s2.lastRestore
	s2.backupMu.Unlock()
	if got == nil || got.Result != backupResultFailed || !strings.Contains(got.Error, "before it was carried out") || !strings.Contains(got.Error, p.Ref) {
		t.Fatalf("outcome = %+v, want the staged-file-still-there answer carrying the ref", got)
	}
	if _, err := os.Stat(restorePendingPath(dbPath)); !os.IsNotExist(err) {
		t.Error("the pending marker should be removed once the result was saved")
	}
	if _, err := os.Stat(restoreStatusPath(dbPath)); err != nil {
		t.Errorf("the result was not persisted: %v", err)
	}

	// Staged file gone: the answer says the outcome is unknown.
	if err := os.WriteFile(restorePendingPath(dbPath), []byte(`{"ref":"r2","file":"x","staged":"gone.restoring-r2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s2.absorbRestoreOutcome()
	s2.backupMu.Lock()
	got = s2.lastRestore
	s2.backupMu.Unlock()
	if got == nil || !strings.Contains(got.Error, "before recording whether it finished") {
		t.Fatalf("outcome = %+v, want the unknown answer", got)
	}
}

// The outcome marker survives a failed save: it is removed only after the saved
// result is in place.
func TestRestore_OutcomeMarkerKeptWhenTheSaveFails(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	if _, err := WriteRestoreOutcome(dbPath, nil, "a caveat"); err != nil {
		t.Fatal(err)
	}
	// A directory where the status file belongs makes the save fail.
	if err := os.Mkdir(restoreStatusPath(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	s.absorbRestoreOutcome()
	if _, err := os.Stat(restoreOutcomePath(dbPath)); err != nil {
		t.Fatalf("the outcome marker must stay for the next start: %v", err)
	}
	s.backupMu.Lock()
	got := s.lastRestore
	s.backupMu.Unlock()
	if got == nil || got.Note != "a caveat" {
		t.Fatalf("in-memory result = %+v, want the outcome kept with its note", got)
	}
}

// The acknowledge endpoint clears the result in memory and on disk.
func TestAcknowledgeRestoreResult_ClearsTheSavedResult(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	if _, err := WriteRestoreOutcome(dbPath, errors.New("boom"), ""); err != nil {
		t.Fatal(err)
	}
	s.absorbRestoreOutcome()
	if _, err := os.Stat(restoreStatusPath(dbPath)); err != nil {
		t.Fatal(err)
	}
	// Without a reference, or with one that is not the shown result's, nothing is cleared.
	for _, q := range []string{"", "?ref=zz", "?ref=0123456789abcdef"} {
		rec := httptest.NewRecorder()
		s.handleAcknowledgeRestore(rec, httptest.NewRequest(http.MethodDelete, "/admin/backup/restore-result"+q, nil))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
			t.Fatalf("ack%q: %d %s, want 400 or 409", q, rec.Code, rec.Body.String())
		}
	}
	if _, err := os.Stat(restoreStatusPath(dbPath)); err != nil {
		t.Fatalf("a refused acknowledgement removed the saved result: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleAcknowledgeRestore(rec, httptest.NewRequest(http.MethodDelete, "/admin/backup/restore-result?ref=-", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(restoreStatusPath(dbPath)); !os.IsNotExist(err) {
		t.Error("the saved result survived the acknowledgement")
	}
	s.backupMu.Lock()
	got := s.lastRestore
	s.backupMu.Unlock()
	if got != nil {
		t.Errorf("in-memory result = %+v, want nil", got)
	}
}

// DecompressStream never writes through a pre-planted file or link at the target.
func TestDecompressStream_CreatesTheTargetExclusively(t *testing.T) {
	dir := t.TempDir()
	var gz bytes.Buffer
	if _, _, err := store.CompressBackup(bytes.NewReader(validSQLiteBytes(t)), &gz); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "out.db")
	if err := os.Symlink(victim, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := store.DecompressStream(context.Background(), bytes.NewReader(gz.Bytes()), target, 1<<30); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "precious" {
		t.Errorf("the file behind the link was overwritten: %q", got)
	}
	if fi, err := os.Lstat(target); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the target should be a fresh regular file, got %v %v", fi, err)
	}
}

// A restore of an in-memory database is refused: there is no file to replace.
func TestRestore_InMemoryDatabaseIsRefused(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil), nil, config.Config{}, st)
	s.SetRestoreChannel(make(chan string, 1))
	rec := restoreRequest(s, "marbor-backup-20260730-120000.db.gz")
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "in-memory") {
		t.Fatalf("restore on an in-memory store: %d %s, want 501 naming the in-memory database", rec.Code, rec.Body.String())
	}
}

// The reservation ledger counts every writer: two reservations on one volume
// cannot both be granted when only one fits, and releasing frees the room.
func TestSpaceLedger_ConcurrentReservationsCannotBothFit(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	// Room for exactly one 5 MiB reservation beyond the margin, not two.
	withFreeBytes(t, 1<<40)
	probe, err := s.roomOn(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	withFreeBytes(t, probe.margin+8<<20)
	h1, granted, _, _, err := s.reserveRoom(dir, 5<<20, true, "test")
	if err != nil || granted != 5<<20 {
		t.Fatalf("first reservation: granted=%d err=%v", granted, err)
	}
	if _, _, _, _, err := s.reserveRoom(dir, 5<<20, true, "test"); !errors.Is(err, errNoRoom) {
		t.Fatalf("second reservation err = %v, want errNoRoom while the first is held", err)
	}
	h1.release()
	h2, _, _, _, err := s.reserveRoom(dir, 5<<20, true, "test")
	if err != nil {
		t.Fatalf("after the first was released: %v", err)
	}
	h2.release()
}

// A failure to read the database size makes the space check refuse, not guess.
func TestRoomOn_FailsClosedWhenTheFreeSpaceCannotBeRead(t *testing.T) {
	s, _ := newBackupTestServer(t)
	old := backupFreeBytes
	backupFreeBytes = func(string) (uint64, error) { return 0, errors.New("statfs failed") }
	defer func() { backupFreeBytes = old }()
	if _, err := s.roomOn(t.TempDir(), "x"); err == nil {
		t.Fatal("an unreadable volume must refuse, not pass")
	}
}

// The verification cache expires, evicts the least recently used entry, and a
// file whose size changed is not served from it.
func TestVerifiedCache_ExpiryEvictionAndIdentity(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	v := &verifiedBackups{now: func() time.Time { return now }}
	mk := func(name, content string) (string, os.FileInfo) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return p, fi
	}
	p, fi := mk("a", "one")
	v.put(p, fi, "", verifiedEntry{fp: "x"})
	if _, ok := v.get(p, fi, ""); !ok {
		t.Fatal("fresh entry not served")
	}
	if _, ok := v.get(p, fi, "other-tail"); ok {
		t.Error("an entry for a different tail must not be served")
	}
	p2, fi2 := mk("a", "changed content")
	if _, ok := v.get(p2, fi2, ""); ok {
		t.Error("an entry for a file of another size must not be served")
	}
	now = now.Add(verifiedMaxAge + time.Minute)
	if _, ok := v.get(p, fi, ""); ok {
		t.Error("an entry older than the expiry must not be served")
	}

	// Eviction: fill past the bound; the oldest-used entry goes.
	v2 := &verifiedBackups{now: func() time.Time { return now }}
	first, ffi := mk("first", "f")
	v2.put(first, ffi, "", verifiedEntry{})
	for i := 0; i < verifiedCacheMax; i++ {
		q, qfi := mk("n"+string(rune('a'+i%26))+strings.Repeat("x", i/26), "q")
		v2.put(q, qfi, "", verifiedEntry{})
	}
	if _, ok := v2.get(first, ffi, ""); ok {
		t.Error("the least recently used entry should have been evicted")
	}
	v2.mu.Lock()
	n := len(v2.m)
	v2.mu.Unlock()
	if n > verifiedCacheMax {
		t.Errorf("cache holds %d entries, bound is %d", n, verifiedCacheMax)
	}
}

// Nothing is saved, and a forged saved file is never read: after a restart
// every file is verified again.
func TestVerifiedCache_IsMemoryOnlyAndIgnoresAForgedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "marbor-backup-20260730-120000.db.gz")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	v := &verifiedBackups{}
	v.put(p, fi, "", verifiedEntry{fp: "fp1", raw: 3})
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("the cache wrote files next to the backups: %d entries", len(ents))
	}
	forged := fmt.Sprintf(`[{"name":%q,"size":3,"mod_ns":%d,"fp":"forged","raw":3,"at":"2999-01-01T00:00:00Z"}]`, filepath.Base(p), fi.ModTime().UnixNano())
	if err := os.WriteFile(filepath.Join(dir, ".marbor-backup-verified.json"), []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	v2 := &verifiedBackups{}
	if _, ok := v2.get(p, fi, ""); ok {
		t.Fatal("a fresh cache served an entry it was never given")
	}
}

// A gzip that lacks the fingerprint record is invalid, not "ok".
func TestVerifyBackupFile_GzipWithoutTailIsInvalid(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	good := writeGz(t, dir, "marbor-backup-20260730-120000.db.gz", validSQLiteBytes(t))
	e, err := s.verifyBackupFile(context.Background(), good)
	if err != nil || e.reason != "" {
		t.Fatalf("a good backup: reason=%q err=%v", e.reason, err)
	}
	raw, _ := os.ReadFile(good)
	cut := filepath.Join(dir, "marbor-backup-20260730-130000.db.gz")
	if err := os.WriteFile(cut, raw[:len(raw)-20], 0o600); err != nil {
		t.Fatal(err)
	}
	e, err = s.verifyBackupFile(context.Background(), cut)
	if err == nil && e.reason == "" {
		t.Fatal("a gzip with a cut-off tail must not verify")
	}
}

// Manual backups are capped, and a slot is given back.
func TestManualBackupSlots_AreCappedAndReleased(t *testing.T) {
	s, _ := newBackupTestServer(t)
	for i := 0; i < maxManualBackups; i++ {
		if !s.acquireManualSlot() {
			t.Fatalf("slot %d refused below the cap", i)
		}
	}
	if s.acquireManualSlot() {
		t.Fatal("a slot was granted above the cap")
	}
	s.releaseManualSlot()
	if !s.acquireManualSlot() {
		t.Fatal("a released slot was not reusable")
	}
}
