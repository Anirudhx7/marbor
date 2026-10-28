package admin

// backup_run.go - housekeeping at start, the scheduler, taking a backup,
// committing it into the backup directory and pruning old ones. Reading files
// back to verify them is in backup_verify.go.
//
// Locks, outermost first: backupRunMu (one backup is being taken),
// backupDirMu (a file is being committed or deleted in the backup directory),
// backupMu (the in-memory status record). Long work - snapshots, compression,
// verification - holds only backupRunMu or nothing, never backupDirMu.
// Retention runs after backupRunMu is released, under a per-pool "running" flag,
// so reading a large directory back never holds up a backup or a download.
//
// The leftover sweep (at start, and when the backup directory changes) takes the
// run and heavy locks only for as long as it removes files, never across slow
// work, and always in the goroutine that releases them. While it runs the
// housekeeping flag is set, and backup, upload and restore requests are refused
// with a message that says why instead of a bare "already running".

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// ---- housekeeping at start ---------------------------------------------------------

var (
	// housekeepingScanTimeout bounds how long listing one directory, or removing
	// one file, may take; a dead network mount must not hold the sweep forever.
	// Replaceable in tests.
	housekeepingScanTimeout = 30 * time.Second

	// sweepRetry is how long a directory sweep waits before trying again when a
	// backup, upload or restore check is using the directory. Replaceable in tests.
	sweepRetry = 15 * time.Second

	// exclusiveWait is how long start-up housekeeping waits for the run and heavy
	// locks before giving up on the cleanup. Replaceable in tests.
	exclusiveWait = 5 * time.Minute

	errFSTimeout = errors.New("the file system did not answer in time")
)

type dirScan struct {
	entries []os.DirEntry
	err     error
}

// scanCall is one directory listing in flight.
type scanCall struct {
	done chan struct{}
	dirScan
}

var (
	scanMu sync.Mutex
	scans  = map[string]*scanCall{}
)

// readDirTimeout lists dir, giving up after housekeepingScanTimeout. A listing
// that hangs on a dead mount stays in flight, and later calls for the same
// directory wait on it instead of starting another goroutine, so a hung mount
// costs one goroutine, not one per call.
func readDirTimeout(dir string) ([]os.DirEntry, error) {
	scanMu.Lock()
	c := scans[dir]
	if c == nil {
		c = &scanCall{done: make(chan struct{})}
		scans[dir] = c
		go func() {
			entries, err := os.ReadDir(dir)
			c.entries, c.err = entries, err
			scanMu.Lock()
			delete(scans, dir)
			scanMu.Unlock()
			close(c.done)
		}()
	}
	scanMu.Unlock()
	select {
	case <-c.done:
		return c.entries, c.err
	case <-time.After(housekeepingScanTimeout):
		return nil, fmt.Errorf("listing the directory took longer than %s", housekeepingScanTimeout)
	}
}

// fsTimeout runs fn, giving up on the answer after housekeepingScanTimeout.
func fsTimeout(fn func() error) error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(housekeepingScanTimeout):
		return errFSTimeout
	}
}

// cleanupBackupLeftovers removes scratch, staging and temporary files that a run
// which died (crash, kill, out-of-memory) left in dir. It returns how many it
// could not remove, and whether the directory could not be read at all (a state
// of its own: it says nothing about leftover files). Only marbor's own file name
// patterns are touched, and only in directories marbor writes to; the shared
// system temp directory is never scanned.
func cleanupBackupLeftovers(dir string) (stuck int, unreadable bool) {
	if dir == "" {
		return 0, false
	}
	entries, err := readDirTimeout(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false
		}
		log.Printf("admin: could not scan the backup directory for leftovers: %v", err)
		return 0, true
	}
	for _, e := range entries {
		if e.IsDir() || !isBackupLeftover(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := fsTimeout(func() error { return os.Remove(path) }); err != nil {
			log.Printf("admin: could not remove leftover backup file %s from an earlier run: %v", e.Name(), err)
			stuck++
			continue
		}
		log.Printf("admin: removed leftover backup file %s from an earlier run", e.Name())
	}
	return stuck, false
}

// tryRun claims "a backup is being taken" without blocking. endRun gives it back.
func (s *Server) tryRun() bool {
	if !s.backupRunMu.TryLock() {
		return false
	}
	s.backupRunSince.Store(time.Now().Unix())
	s.backupRunning.Store(true)
	return true
}

func (s *Server) endRun() {
	s.backupRunning.Store(false)
	s.backupRunSince.Store(0)
	s.backupRunMu.Unlock()
}

// tryHeavy claims "an upload or a restore check is running" without blocking.
// endHeavy gives it back.
func (s *Server) tryHeavy() bool {
	if !s.backupHeavyMu.TryLock() {
		return false
	}
	s.heavyRunning.Store(true)
	return true
}

func (s *Server) endHeavy() {
	s.heavyRunning.Store(false)
	s.backupHeavyMu.Unlock()
}

// goBackground runs fn in a goroutine that Shutdown (through StopBackups) waits
// for, with a context that ends when backups are stopped, and recovers a panic so
// no lock it holds is ever leaked by one. Nothing starts once backups are
// stopping.
func (s *Server) goBackground(what string, fn func(ctx context.Context)) {
	s.bgMu.Lock()
	if s.backupsStopping.Load() {
		s.bgMu.Unlock()
		return
	}
	s.schedWG.Add(1)
	s.bgMu.Unlock()
	ctx := s.schedContext()
	go func() {
		defer s.schedWG.Done()
		defer recoverLog(what)
		fn(ctx)
	}()
}

// tryExclusive takes the run and heavy locks together, or neither.
func (s *Server) tryExclusive() bool {
	if !s.backupRunMu.TryLock() {
		return false
	}
	if !s.backupHeavyMu.TryLock() {
		s.backupRunMu.Unlock()
		return false
	}
	return true
}

func (s *Server) unlockExclusive() {
	s.backupHeavyMu.Unlock()
	s.backupRunMu.Unlock()
}

// waitExclusive polls for tryExclusive until it succeeds, ctx ends or d passes.
func (s *Server) waitExclusive(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if s.tryExclusive() {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// runBackupHousekeeping loads the saved backup status, picks up the outcome of
// a restore that ended the previous process, and clears what that process left
// behind. The restore outcome is read before staged restore files are removed:
// whether the staged file still exists is part of what it reports. The leftover
// removal waits (bounded) for any running backup, which holds nothing at start.
func (s *Server) runBackupHousekeeping(ctx context.Context, targetDir string) {
	statusDir := s.statusDirFor(targetDir)
	s.prepareBackupStatus(statusDir)
	s.absorbRestoreOutcome()

	if !s.waitExclusive(ctx, exclusiveWait) {
		if ctx.Err() == nil {
			log.Printf("admin: start-up cleanup of the backup directory skipped: a backup or upload is still running")
		}
		return
	}
	defer s.unlockExclusive()
	s.sweepLeftovers(targetDir)
}

// sweepLeftovers removes the leftovers of the backup directory, the database
// directory and the staged restore files, and records what it found. The caller
// holds the run and heavy locks.
func (s *Server) sweepLeftovers(targetDir string) {
	statusDir := s.statusDirFor(targetDir)
	dbDir := s.dirOfDB()
	stuck, unreadable := cleanupBackupLeftovers(targetDir)
	if dbDir != "" && dbDir != targetDir {
		n, u := cleanupBackupLeftovers(dbDir)
		stuck += n
		unreadable = unreadable || u
	}
	stuck += cleanupStagedRestores(s.st.DBPath())
	s.recordLeftovers(statusDir, stuck, unreadable)
}

// SweepBackupDir clears marbor's leftover files from dir, which has just been
// configured as the backup directory. It never blocks the settings request or
// a backup: when a backup, upload or restore check is using the directory it
// waits and tries again, and it stops at shutdown. It is tracked with the
// scheduler, which Shutdown waits for.
func (s *Server) SweepBackupDir(dir string) {
	if dir == "" {
		return
	}
	s.goBackground("backup directory sweep", func(ctx context.Context) {
		for {
			if ctx.Err() != nil || s.currentTargetDir() != dir {
				return // stopping, or changed again while waiting
			}
			if s.trySweep(dir) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(sweepRetry):
			}
		}
	})
}

// trySweep sweeps dir if nothing is using it, and reports whether it did.
func (s *Server) trySweep(dir string) bool {
	if !s.housekeeping.CompareAndSwap(false, true) {
		return false
	}
	defer s.housekeeping.Store(false)
	if !s.tryExclusive() {
		return false
	}
	defer s.unlockExclusive()
	stuck, unreadable := cleanupBackupLeftovers(dir)
	s.recordLeftovers(s.statusDirFor(dir), stuck, unreadable)
	return true
}

// recoverLog keeps a panic in a background goroutine from taking marbor down;
// the deferred unlocks above it have already run by then.
func recoverLog(what string) {
	if r := recover(); r != nil {
		log.Printf("admin: %s stopped after a panic: %v", what, r)
	}
}

// schedContext is the context that scheduler runs and sweeps use: cancelled when
// backups are stopped. Before StartBackupScheduler (tests) it never ends.
func (s *Server) schedContext() context.Context {
	s.mu.RLock()
	ctx := s.schedCtx
	s.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ---- snapshot and archive -------------------------------------------------------

func (s *Server) removeScratch(path string) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("admin: could not remove scratch file %s: %v", filepath.Base(path)+suffix, err)
			s.scratchLeftovers.Add(1)
		}
	}
}

// takeSnapshot writes a consistent plain snapshot of the live database into a
// fresh 0600 scratch file under scratchDir. The returned cleanup removes it and
// must always be called. Cancelling ctx interrupts the snapshot.
func (s *Server) takeSnapshot(ctx context.Context, scratchDir string) (string, func(), error) {
	f, err := os.CreateTemp(scratchDir, "marbor-backup-scratch-*.db")
	if err != nil {
		return "", func() {}, newBackupFailure("could not create the snapshot file", err)
	}
	path := f.Name()
	cleanup := func() { s.removeScratch(path) }
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, newBackupFailure("could not create the snapshot file", err)
	}
	if err := s.st.BackupTo(ctx, path); err != nil {
		cleanup()
		if cerr := ctx.Err(); cerr != nil {
			return "", func() {}, cerr
		}
		return "", func() {}, newBackupFailure("could not take a snapshot of the database", err)
	}
	return path, cleanup, nil
}

// writeBackupArchive takes a snapshot under scratchDir and compresses it into a
// new 0600 temporary file in outDir, syncing it to disk. It returns that file's
// path and the canonical fingerprint and size of the snapshot. Once the snapshot
// exists its real size is checked against the room left for the compressed copy
// (see checkOutputRoom). On any failure the temporary file is removed. It stops
// early when ctx is cancelled.
func (s *Server) writeBackupArchive(ctx context.Context, scratchDir, outDir, pattern string, ticket *spaceTicket) (tmpPath, fingerprint string, rawBytes int64, err error) {
	out, err := os.CreateTemp(outDir, pattern)
	if err != nil {
		return "", "", 0, newBackupFailure("could not create the backup file", err)
	}
	created := out.Name()
	closed := false
	defer func() {
		if !closed {
			out.Close()
		}
		if err != nil {
			if rmErr := os.Remove(created); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				log.Printf("admin: could not remove the unfinished backup file %s: %v", filepath.Base(created), rmErr)
			}
		}
	}()

	snap, cleanup, err := s.takeSnapshot(ctx, scratchDir)
	if err != nil {
		return "", "", 0, err
	}
	defer cleanup()
	if cerr := ctx.Err(); cerr != nil {
		return "", "", 0, cerr
	}
	snapInfo, err := os.Stat(snap)
	if err != nil {
		return "", "", 0, newBackupFailure("could not read the snapshot", err)
	}
	if err := s.checkOutputRoom(outDir, snapInfo.Size(), ticket); err != nil {
		return "", "", 0, err
	}

	src, err := os.Open(snap)
	if err != nil {
		return "", "", 0, newBackupFailure("could not read the snapshot", err)
	}
	fingerprint, rawBytes, err = store.CompressBackup(ctxReader{ctx, src}, out)
	if cerr := src.Close(); err == nil && cerr != nil {
		err = newBackupFailure("could not read the snapshot", cerr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", "", 0, ctx.Err()
		}
		if isDiskFull(err) {
			return "", "", 0, &backupRefusedError{msg: "backup refused: the disk filled up while the backup was being written"}
		}
		var failure *backupFailure
		if errors.As(err, &failure) {
			return "", "", 0, err
		}
		return "", "", 0, newBackupFailure("could not write the compressed backup", err)
	}
	cleanup() // free the raw snapshot before verification
	if err := out.Sync(); err != nil {
		return "", "", 0, newBackupFailure("could not flush the backup file to disk", err)
	}
	closed = true
	if err := out.Close(); err != nil {
		return "", "", 0, newBackupFailure("could not close the backup file", err)
	}
	return created, fingerprint, rawBytes, nil
}

// verifyArchive reads the finished archive back from disk: every gzip checksum
// must pass, the decompressed size and content fingerprint must equal what was
// written, and the embedded fingerprint record must match. The result is not
// cached: the file still has its temporary name, and it is cached under its final
// name once it is committed.
func (s *Server) verifyArchive(ctx context.Context, path, fingerprint string, rawBytes int64) error {
	e, err := s.verifyBackupFileOpts(ctx, path, false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return newBackupFailure("could not read the backup back to verify it; it was discarded", err)
	}
	switch {
	case e.reason != "":
		return newBackupFailure("the written backup failed verification and was discarded", errors.New(e.reason))
	case e.fp != fingerprint || e.raw != rawBytes:
		return newBackupFailure("the written backup does not match the database snapshot and was discarded",
			fmt.Errorf("fingerprint/size mismatch: wrote %s/%d, read back %s/%d", fingerprint, rawBytes, e.fp, e.raw))
	}
	return nil
}

// ---- committing into the backup directory ---------------------------------------

// backupNameTaken reports whether base (with either extension) already exists
// in dir. A directory that cannot be inspected is an error, not "taken": every
// name would look taken and the real cause (permissions, a dead mount) would be
// reported as a name collision.
func backupNameTaken(dir, base string) (bool, error) {
	for _, ext := range []string{".db", ".db.gz"} {
		_, err := os.Lstat(filepath.Join(dir, base+ext))
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, os.ErrNotExist):
			return false, err
		}
	}
	return false, nil
}

// linkFile makes a hard link. It is a variable so a test can stand in for a
// filesystem that has none (some network and FAT-style mounts).
var linkFile = os.Link

// commitBackupFile moves the finished temporary file tmpPath into dir as a kept
// backup and returns its name. The name is the current UTC time (moved forward
// a second at a time on a collision), never an existing file: the whole step
// holds backupDirMu, so scheduled runs, uploads and pruning cannot interleave.
// The file is hard-linked into place where the filesystem allows, which fails
// rather than replaces if the name appears from outside marbor. Where the
// filesystem has no hard links, the name is checked again immediately before a
// plain rename; that leaves a window of a few microseconds in which a process
// outside marbor creating exactly that timestamped name could be overwritten,
// which the directory lock closes against every writer inside marbor. The
// directory is synced afterwards.
//
// A non-empty name with a non-nil error means the backup is in place and only
// something around it failed (the directory could not be flushed, or the
// temporary name could not be removed after linking); the error text says what.
func (s *Server) commitBackupFile(dir, tmpPath, ext string, uploaded bool) (string, error) {
	s.backupDirMu.Lock()
	defer s.backupDirMu.Unlock()
	start := time.Now()
	for i := 0; i < maxBackupNameCollisions; i++ {
		base := backupBaseName(start.Add(time.Duration(i) * time.Second))
		if uploaded {
			base += "-upload"
		}
		taken, terr := backupNameTaken(dir, base)
		if terr != nil {
			return "", newBackupFailure("could not check the backup directory for an existing backup file", terr)
		}
		if taken {
			continue
		}
		final := filepath.Join(dir, base+ext)
		var warn error
		linkErr := linkFile(tmpPath, final)
		switch {
		case linkErr == nil:
			if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				warn = newBackupFailure("the backup was saved but a temporary copy of it could not be removed; it is cleaned up at the next start", rmErr)
			}
		case errors.Is(linkErr, os.ErrExist):
			continue
		default:
			if taken, terr := backupNameTaken(dir, base); terr != nil {
				return "", newBackupFailure("could not check the backup directory for an existing backup file", terr)
			} else if taken {
				continue
			}
			if err := os.Rename(tmpPath, final); err != nil {
				return "", newBackupFailure("could not save the backup file", err)
			}
		}
		if err := syncDir(dir); err != nil {
			return base + ext, errors.Join(warn, newBackupFailure("the backup was saved but the directory could not be flushed to disk", err))
		}
		return base + ext, warn
	}
	return "", newBackupFailure("could not find a free backup file name", nil)
}

// commitWarning is the operator-safe text for the warning half of a commit.
func commitWarning(err error) string {
	var failure *backupFailure
	if errors.As(err, &failure) {
		return failure.public
	}
	log.Printf("admin: backup saved with a warning: %v", err)
	return "the backup was saved with a warning (see the marbor log)"
}

// ---- scheduling ------------------------------------------------------------------

// scheduledBackupDue reports whether a scheduled run should start now.
func scheduledBackupDue(now, lastSuccess, lastAttempt time.Time, lastResult string, intervalHours int) bool {
	if intervalHours <= 0 {
		intervalHours = 24
	}
	if (lastResult == backupResultFailed || lastResult == backupResultRefused) && !lastAttempt.IsZero() && now.Sub(lastAttempt) < backupFailureBackoff {
		return false
	}
	return now.Sub(lastSuccess) >= time.Duration(intervalHours)*time.Hour
}

// StartBackupScheduler launches a background goroutine that runs a scheduled
// marbor.db backup into cfg.Backup.TargetDir whenever it is enabled and due,
// then prunes old backup files beyond cfg.Backup.RetentionCount. The start-up
// housekeeping (loading the saved status, picking up the outcome of a restore,
// removing what a run that died left behind) runs first, in that goroutine.
// Until it finishes the housekeeping flag is set and backup, upload and restore
// requests are refused with a message that says so; no lock is held across it.
// A second call does nothing. ctx cancellation, or StopBackups, stops the
// ticker and any running backup, and Shutdown waits for the goroutine.
func (s *Server) StartBackupScheduler(ctx context.Context) {
	if !s.schedStarted.CompareAndSwap(false, true) {
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.schedCtx, s.schedCancel = sctx, cancel
	s.mu.Unlock()
	s.housekeeping.Store(true)
	s.bgMu.Lock()
	if s.backupsStopping.Load() {
		s.bgMu.Unlock()
		s.housekeeping.Store(false)
		return
	}
	s.schedWG.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.schedWG.Done()
		defer recoverLog("backup scheduler")
		func() {
			defer s.housekeeping.Store(false)
			defer recoverLog("start-up backup housekeeping")
			s.runBackupHousekeeping(sctx, s.currentTargetDir())
		}()
		wait := backupCheckInterval
		if sctx.Err() == nil && s.scheduledCheck(sctx) {
			wait = backupDeferRetry // in case a backup came due while marbor was stopped
		}
		for {
			select {
			case <-sctx.Done():
				return
			case <-time.After(wait):
			}
			wait = backupCheckInterval
			if s.scheduledCheck(sctx) {
				wait = backupDeferRetry
			}
		}
	}()
}

// scheduledCheck runs one scheduler wake-up and reports whether a due backup was
// deferred (to be retried shortly). A panic in it is logged and the scheduler
// carries on at the next wake-up.
func (s *Server) scheduledCheck(ctx context.Context) (deferred bool) {
	defer recoverLog("scheduled backup check")
	s.mu.RLock()
	cfg := s.cfg.Backup
	s.mu.RUnlock()
	if !cfg.Enabled {
		return false
	}
	lastSuccess, lastAttempt, lastResult := s.scheduleState(s.statusDirFor(cfg.TargetDir))
	if !scheduledBackupDue(time.Now(), lastSuccess, lastAttempt, lastResult, cfg.IntervalHours) {
		return false
	}
	err := s.runScheduledBackup(ctx, cfg)
	switch {
	case errors.Is(err, errBackupDeferred), errors.Is(err, errBackupBusy):
		return true
	case err != nil:
		log.Printf("admin: scheduled backup did not complete: %v", err)
	}
	return false
}

// schedulerShutdownWait is how long StopBackups waits for a running scheduled
// backup to notice that it was cancelled and record how it ended.
var schedulerShutdownWait = 20 * time.Second

// StopBackups cancels the scheduler and any scheduled or swept work, stops new
// backups from starting, and waits (bounded) for them to end. main calls it as
// soon as a restore is accepted, before the servers drain, so no snapshot is
// holding the database open when it has to be closed and replaced.
func (s *Server) StopBackups() {
	s.backupsStopping.Store(true)
	s.bgMu.Lock() // barrier: after this no goroutine is added to the wait group
	s.bgMu.Unlock()
	s.mu.RLock()
	cancel := s.schedCancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	s.waitScheduler()
}

// waitScheduler blocks until the scheduler goroutine has finished, or the wait
// is over. When a scheduled backup is still running at that point its outcome is
// recorded here, as interrupted by the shutdown, so the next start does not show
// a stale "success".
func (s *Server) waitScheduler() {
	done := make(chan struct{})
	go func() {
		s.schedWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(schedulerShutdownWait):
		log.Printf("WARNING: the backup scheduler did not stop within %s of shutdown", schedulerShutdownWait)
		if s.schedRunActive.Load() {
			s.recordBackupOutcome(s.statusDirFor(s.currentTargetDir()), backupSourceScheduled, context.Canceled)
		}
	}
}

// logSkippedScheduled notes that a due scheduled run was skipped because
// another backup is running, at most once per backupSkipLogEvery.
func (s *Server) logSkippedScheduled(reason string) {
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if time.Since(s.backupSkipLoggedAt) < backupSkipLogEvery {
		return
	}
	s.backupSkipLoggedAt = time.Now()
	log.Printf("admin: a scheduled backup is due but %s; it will be retried shortly", reason)
}

// runScheduledBackup performs one scheduled backup run and records its outcome
// (success, failed, or refused for lack of disk space), then prunes old backups
// beyond cfg.RetentionCount - after the backup lock is released, so reading the
// directory back never holds up the next backup. A run that cannot start because
// the directory is being swept, or another backup, upload or restore is using
// the disk, is deferred, not failed: it is noted on the status and retried
// within a minute, with no backoff.
func (s *Server) runScheduledBackup(ctx context.Context, cfg config.BackupConfig) error {
	statusDir := s.statusDirFor(cfg.TargetDir)
	if cfg.TargetDir == "" {
		err := newBackupFailure("no backup target directory is configured", nil)
		s.recordBackupOutcome(statusDir, backupSourceScheduled, err)
		return err
	}
	if s.backupsStopping.Load() {
		return errBackupDeferred
	}
	if s.housekeeping.Load() {
		s.noteScheduledDeferred(statusDir, "the backup directory is being checked for leftover files")
		s.logSkippedScheduled("the backup directory is being checked for leftover files")
		return fmt.Errorf("%w: housekeeping", errBackupDeferred)
	}
	if s.heavyRunning.Load() {
		s.noteScheduledDeferred(statusDir, "an upload or restore check is using the disk")
		s.logSkippedScheduled("an upload or restore check is running")
		return fmt.Errorf("%w: upload or restore check", errBackupDeferred)
	}
	if !s.tryRun() {
		s.logSkippedScheduled("another backup is still running")
		return errBackupBusy
	}
	s.schedRunActive.Store(true)
	warning, err := func() (string, error) {
		defer s.endRun()
		defer s.schedRunActive.Store(false)
		return s.scheduledBackupLocked(ctx, cfg.TargetDir)
	}()
	if errors.Is(err, errBackupDeferred) {
		s.noteScheduledDeferred(statusDir, "another backup operation is using the disk")
		s.logSkippedScheduled("another backup operation is using the disk")
		return err
	}
	s.recordBackupOutcomeWarn(statusDir, backupSourceScheduled, err, warning)
	if err == nil {
		s.runPrune(ctx, cfg.TargetDir, cfg.RetentionCount, false)
	}
	return err
}

// scheduledBackupLocked takes one scheduled backup. The warning is non-empty for
// a backup that is kept but whose directory could not be flushed (or whose
// temporary name could not be removed): the file is in place, so the run is a
// success that says so.
func (s *Server) scheduledBackupLocked(ctx context.Context, dir string) (warning string, err error) {
	if err := ensureBackupDir(dir); err != nil {
		return "", newBackupFailure("could not create the backup target directory", err)
	}
	if _, w := backupDirState(dir); w != "" {
		log.Printf("admin: %s", w)
	}
	ticket, err := s.backupPreflight(dir, dir)
	if err != nil {
		return "", err
	}
	defer ticket.release()
	tmp, fp, raw, err := s.writeBackupArchive(ctx, dir, dir, "marbor-backup-write-*.tmp", ticket)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				log.Printf("admin: could not remove the temporary backup file %s: %v", filepath.Base(tmp), rmErr)
			}
		}
	}()

	// Skip a backup identical to one already kept among the scheduled backups.
	// The fingerprint recorded in a kept file only nominates it; the candidate is
	// then read in full and its computed fingerprint compared, so a damaged file
	// is never mistaken for a good copy. A candidate that fails the check is
	// logged and ignored: the new backup is kept. Uploaded files are another
	// pool and never stand in for a scheduled backup.
	if dup := s.findDuplicateBackup(ctx, dir, fp, raw, false, true); dup != "" {
		log.Printf("admin: scheduled backup skipped: the database is unchanged since %s", dup)
		return "", nil
	}
	if err := s.verifyArchive(ctx, tmp, fp, raw); err != nil {
		return "", err
	}
	name, err := s.commitBackupFile(dir, tmp, ".db.gz", false)
	if name == "" {
		return "", err
	}
	committed = true // the file is in place (even if something around it failed)
	s.noteVerified(filepath.Join(dir, name), fp, raw)
	if err != nil {
		return commitWarning(err), nil
	}
	return "", nil
}

// ---- retention ---------------------------------------------------------------------

// pruneRequest is a retention pass asked for while one was already running.
type pruneRequest struct {
	pending   bool
	dir       string
	retention int
}

// prunePassDone, when set, is called after each retention pass. A test seam.
var prunePassDone func()

// runPrune runs one retention pass over a pool and records its result. When a
// pass is already running for the pool, the request is remembered and a single
// further pass runs when that one ends: the files the running pass looked at
// may not include the one that was just added.
func (s *Server) runPrune(ctx context.Context, dir string, retention int, uploaded bool) {
	idx := 0
	if uploaded {
		idx = 1
	}
	s.backupMu.Lock()
	if s.pruneRunning[idx] {
		s.prunePending[idx] = pruneRequest{pending: true, dir: dir, retention: retention}
		s.backupMu.Unlock()
		return
	}
	s.pruneRunning[idx] = true
	s.backupMu.Unlock()
	for {
		problem := s.pruneBackups(ctx, dir, retention, uploaded)
		if prunePassDone != nil {
			prunePassDone()
		}
		if ctx.Err() == nil {
			s.recordPruneResult(s.statusDirFor(dir), uploaded, problem)
		}
		s.backupMu.Lock()
		req := s.prunePending[idx]
		s.prunePending[idx] = pruneRequest{}
		if !req.pending || ctx.Err() != nil {
			s.pruneRunning[idx] = false
			s.backupMu.Unlock()
			return
		}
		s.backupMu.Unlock()
		dir, retention = req.dir, req.retention
	}
}

// removeBackupFile deletes one backup. A file that is already gone counts as
// removed. On Windows a file another process has open (an antivirus scan, a
// download) cannot be deleted for a moment, so the removal is retried briefly.
func removeBackupFile(path string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 4
	}
	var err error
	for i := 0; i < attempts; i++ {
		err = os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if i+1 < attempts {
			time.Sleep(250 * time.Millisecond)
		}
	}
	return err
}

// deletion is a backup chosen for removal and what it looked like when it was
// verified.
type deletion struct {
	name string
	id   os.FileInfo
}

// pruneBackups deletes the oldest verified-good backups beyond retention among
// the scheduled-and-legacy files, or among uploaded files when uploaded is set
// (uploads have their own budget of the same size, so uploading cannot push
// genuine scheduled backups out; the directory can hold up to twice the
// retention count). The count is checked first: a pool within its limit is not
// read at all. Beyond it, files are verified newest first (a verified file is
// remembered, so a steady-state pass reads only files it has not seen); only
// files that read back in full and pass the check count toward the budget: a
// damaged file never takes the place of a good one, and is kept (flagged
// "invalid" in the list, and removable with the delete action) rather than
// deleted. A file that cannot be read is skipped, not counted, and does not stop
// the others from being pruned. retention <= 0 disables pruning. It returns a
// short description of what it could not do, or "".
func (s *Server) pruneBackups(ctx context.Context, dir string, retention int, uploaded bool) string {
	if retention <= 0 {
		return ""
	}
	names, err := listBackupNames(dir)
	if err != nil {
		log.Printf("admin: could not list the backup directory for pruning: %v", err)
		return "old backups could not be pruned: the backup directory could not be listed"
	}
	var pool []string
	for _, name := range names {
		if isUploadedBackupName(name) == uploaded {
			pool = append(pool, name)
		}
	}
	if len(pool) <= retention {
		return "" // within the limit: nothing can be deleted, so nothing is read
	}

	var excess []deletion
	kept := 0
	unverifiable, damaged := 0, 0
	var damagedBytes int64
	for i := len(pool) - 1; i >= 0; i-- { // newest first
		name := pool[i]
		path := filepath.Join(dir, name)
		e, verr := s.verifyBackupFile(ctx, path)
		switch {
		case verr != nil:
			if ctx.Err() != nil {
				return ""
			}
			log.Printf("admin: could not verify backup %s for pruning: %v", name, verr)
			unverifiable++
		case e.reason != "":
			log.Printf("admin: backup %s is damaged (%s); it does not count toward retention and was left in place", name, e.reason)
			damaged++
			if fi, serr := os.Lstat(path); serr == nil {
				damagedBytes += fi.Size()
			}
		case kept < retention:
			kept++
		default:
			excess = append(excess, deletion{name: name, id: e.id})
		}
	}

	var problems []string
	if unverifiable > 0 {
		problems = append(problems, fmt.Sprintf("%d file(s) could not be read to verify them and were skipped", unverifiable))
	}
	if damaged > 0 {
		problems = append(problems, fmt.Sprintf("%d damaged backup(s) using %s are kept and do not count toward the limit; delete them from the list or with marbor backup delete", damaged, formatBytes(uint64(max(damagedBytes, 0)))))
	}
	if len(excess) > 0 {
		if failed := s.deleteExcess(dir, excess); failed > 0 {
			problems = append(problems, fmt.Sprintf("%d old backup(s) could not be deleted", failed))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return "retention: " + strings.Join(problems, "; ")
}

// deleteExcess removes the listed backups under the directory lock, leaving any
// that is in use (pinned) or is no longer the file that was verified (its name
// now points at a different file, or the file changed). It returns how many it
// tried and failed to delete.
func (s *Server) deleteExcess(dir string, list []deletion) (failed int) {
	s.backupDirMu.Lock()
	defer s.backupDirMu.Unlock()
	for _, d := range list {
		if s.isPinned(d.name) {
			continue
		}
		path := filepath.Join(dir, d.name)
		if d.id != nil {
			cur, err := os.Lstat(path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					log.Printf("admin: could not re-check old backup %s before deleting it: %v", d.name, err)
					failed++
				}
				continue
			}
			if !os.SameFile(d.id, cur) || cur.Size() != d.id.Size() || !cur.ModTime().Equal(d.id.ModTime()) {
				log.Printf("admin: old backup %s changed after it was verified; leaving it for the next pass", d.name)
				continue
			}
		}
		if err := removeBackupFile(path); err != nil {
			log.Printf("admin: failed to prune old backup %s: %v", d.name, err)
			failed++
			continue
		}
		s.backupVerified.forget(path)
	}
	if err := syncDir(dir); err != nil {
		log.Printf("admin: could not flush the backup directory after pruning: %v", err)
	}
	return failed
}

// errBackupInUse means a backup cannot be deleted because a restore or a download
// is reading it.
var errBackupInUse = errors.New("the backup is in use")

// deleteBackup removes one backup file by name, for the operator. A backup that
// a restore or a download is reading is refused, and a name that matches no file
// is os.ErrNotExist.
func (s *Server) deleteBackup(dir, name string) error {
	s.backupDirMu.Lock()
	defer s.backupDirMu.Unlock()
	if s.isPinned(name) {
		return errBackupInUse
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errNotRegularBackup
	}
	if err := removeBackupFile(path); err != nil {
		return err
	}
	s.backupVerified.forget(path)
	if err := syncDir(dir); err != nil {
		log.Printf("admin: could not flush the backup directory after deleting %s: %v", name, err)
	}
	return nil
}

// pinBackup marks name as in use (a restore or a download is reading it), so
// pruning leaves it alone, and returns the function that releases it. Pins
// count, so two readers of one file do not release each other's.
func (s *Server) pinBackup(name string) func() {
	s.backupMu.Lock()
	if s.backupPins == nil {
		s.backupPins = map[string]int{}
	}
	s.backupPins[name]++
	s.backupMu.Unlock()
	return func() {
		s.backupMu.Lock()
		if s.backupPins[name]--; s.backupPins[name] <= 0 {
			delete(s.backupPins, name)
		}
		s.backupMu.Unlock()
	}
}

// isPinned reports whether a restore or download is using name. It is called
// under backupDirMu, immediately before deleting, so a pin taken after the
// retention pass started still protects the file.
func (s *Server) isPinned(name string) bool {
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	return s.backupPins[name] > 0
}

// listBackupNames returns the backup file names in dir, oldest first.
func listBackupNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && backupFilenameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		ki, kj := backupSortKey(names[i]), backupSortKey(names[j])
		if ki != kj {
			return ki < kj
		}
		return names[i] < names[j]
	})
	return names, nil
}

// findDuplicateBackup returns the name of a kept backup whose verified content
// equals fp (rawBytes decompressed bytes), or "". A compressed backup is
// nominated by the fingerprint in its tail and then confirmed by reading it in
// full; a plain backup is nominated by size and confirmed by hashing it, but
// only when includePlain is set. skipUploaded leaves the uploads pool out (a
// scheduled backup is compared with scheduled backups only). Confirmations are
// cached, so repeated checks of an unchanged directory read nothing. A
// candidate that cannot be confirmed is logged and skipped.
func (s *Server) findDuplicateBackup(ctx context.Context, dir, fp string, rawBytes int64, includePlain, skipUploaded bool) string {
	names, err := listBackupNames(dir)
	if err != nil {
		log.Printf("admin: could not list the backup directory to look for a duplicate: %v", err)
		return ""
	}
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		if skipUploaded && isUploadedBackupName(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if strings.HasSuffix(name, ".gz") {
			if hint, ok := store.ReadFingerprintTail(path); !ok || hint != fp {
				continue
			}
		} else {
			if !includePlain {
				continue
			}
			if fi, serr := os.Lstat(path); serr != nil || !fi.Mode().IsRegular() || fi.Size() != rawBytes {
				continue
			}
		}
		e, verr := s.verifyBackupFile(ctx, path)
		switch {
		case verr != nil:
			log.Printf("admin: could not verify %s while looking for a duplicate: %v", name, verr)
		case e.reason != "":
			log.Printf("admin: %s looks like a duplicate but failed verification (%s); keeping the new backup", name, e.reason)
		case e.fp == fp && e.raw == rawBytes:
			return name
		}
	}
	return ""
}

// copyAll is io.Copy with a 1 MiB buffer that never uses the destination's
// ReadFrom, so a deadline wrapper sees every chunk.
func copyAll(dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(dst, struct{ io.Reader }{src}, make([]byte, 1<<20))
}
