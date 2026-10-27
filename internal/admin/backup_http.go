package admin

// backup_http.go - the list, download and on-demand backup handlers.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// setDownloadHeaders marks a response as a one-off download of sensitive data:
// not cacheable by the browser or a proxy, and not sniffed into another type.
func setDownloadHeaders(h http.Header, contentType, filename string) {
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	h.Set("X-Content-Type-Options", "nosniff")
}

func backupContentType(name string) string {
	if strings.HasSuffix(name, ".gz") {
		return "application/gzip"
	}
	return "application/octet-stream"
}

// handleListBackups lists the backup files sitting in cfg.Backup.TargetDir,
// newest first, plus the backup status and any directory warning.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	targetDir := s.currentTargetDir()

	files := []backupFileInfo{}
	skipped := 0
	if targetDir != "" {
		names, err := listBackupNames(targetDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			writeServerError(w, r, err)
			return
		}
		for i := len(names) - 1; i >= 0; i-- {
			path := filepath.Join(targetDir, names[i])
			info, err := os.Lstat(path)
			if err != nil {
				log.Printf("admin: could not inspect backup file %s: %v", names[i], err)
				skipped++
				continue
			}
			if !info.Mode().IsRegular() {
				log.Printf("admin: ignoring %s in the backup directory: not a regular file", names[i])
				skipped++
				continue
			}
			files = append(files, backupFileInfo{
				Name: names[i], SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC(),
				Compressed: strings.HasSuffix(names[i], ".gz"),
				Uploaded:   isUploadedBackupName(names[i]),
				Integrity:  s.integrityOf(path, names[i]),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"backups": files, "status": s.backupStatusSnapshot(targetDir), "skipped": skipped}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("admin: could not write the backup list response: %v", err)
	}
}

// handleDownloadBackup streams one listed backup file to the client. The name
// must be a bare marbor-backup-<ts>.db or .db.gz name that exists as a regular
// file in the target directory - never a path.
func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || filepath.Base(name) != name || !backupFilenameRE.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid backup filename")
		return
	}
	targetDir := s.currentTargetDir()
	if targetDir == "" {
		writeJSONError(w, http.StatusBadRequest, "no backup target directory configured")
		return
	}
	f, fi, err := openBackupFile(filepath.Join(targetDir, name))
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errNotRegularBackup):
		writeJSONError(w, http.StatusNotFound, "backup file not found")
		return
	default:
		writeCorrelatedError(w, r, http.StatusInternalServerError, "could not open the backup file", err)
		return
	}
	defer f.Close()
	// A retention pass must not delete the file while it is being sent.
	defer s.pinBackup(name)()

	// A download can run for minutes; the connection's fixed timeouts give way to
	// an idle timeout that only fires when no bytes move. The body goes through
	// idleWriter on purpose: it renews the deadline before every chunk, which a
	// direct file-to-socket copy (sendfile) would bypass, so a transfer that keeps
	// moving is never cut and one that stalls is.
	dc := s.newDeadlineControl(w)
	dc.clearRead()
	setDownloadHeaders(w.Header(), backupContentType(name), name)
	cw := &countingWriter{ResponseWriter: idleWriter{ResponseWriter: w, dc: dc}}
	http.ServeContent(cw, r, name, fi.ModTime(), f)
	// Logged after the transfer, with what actually moved and the status that was
	// sent: a download that the client abandoned must not read as a completed one
	// in the system log, and a range request is logged as the part it was.
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}
	switch {
	case status == http.StatusPartialContent:
		s.logSystemChange(r, "download_backup", "global", fmt.Sprintf("Downloaded part of backup %s (status %d, %s of %s sent)", name, status, formatBytes(uint64(cw.n)), formatBytes(uint64(fi.Size()))))
	case status == http.StatusOK && cw.n == fi.Size():
		s.logSystemChange(r, "download_backup", "global", fmt.Sprintf("Downloaded backup %s (%s sent)", name, formatBytes(uint64(cw.n))))
	case status == http.StatusOK:
		s.logSystemChange(r, "download_backup", "global", fmt.Sprintf("Download of backup %s was interrupted after %s of %s", name, formatBytes(uint64(cw.n)), formatBytes(uint64(fi.Size()))))
	default:
		s.logSystemChange(r, "download_backup", "global", fmt.Sprintf("Download of backup %s ended with status %d (%s sent)", name, status, formatBytes(uint64(cw.n))))
	}
}

// countingWriter counts the body bytes written through it and remembers the
// status code of the response.
type countingWriter struct {
	http.ResponseWriter
	n      int64
	status int
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// maxManualBackups is how many on-demand backups may be in flight at once,
// counting the build and the download that follows it. Each one holds a
// database-sized file on disk until its download ends, so an unbounded number
// of slow clients could fill the volume; the shared space ledger covers the
// build, and this cap covers the download.
const maxManualBackups = 2

// acquireManualSlot takes one of the on-demand backup slots without blocking.
func (s *Server) acquireManualSlot() bool {
	s.manualSlotsOnce.Do(func() { s.manualSlots = make(chan struct{}, maxManualBackups) })
	select {
	case s.manualSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseManualSlot() { <-s.manualSlots }

// manualScratchDir is where an on-demand backup is built: the backup directory
// when there is one (so the space check and the leftover cleanup cover it),
// otherwise the directory holding the database. Only an in-memory store (tests,
// demo) falls back to the system temp directory, which can be RAM-backed; the
// choice is logged. The second return value names the choice for the log.
func (s *Server) manualScratchDir(dir string) (string, string) {
	if dir != "" {
		if err := ensureBackupDir(dir); err == nil {
			return dir, "the backup directory"
		} else {
			log.Printf("admin: on-demand backup cannot use the backup directory, falling back: %v", err)
		}
	}
	if d := s.dirOfDB(); d != "" {
		return d, "the database directory"
	}
	return os.TempDir(), "the system temp directory (in-memory database)"
}

// buildManualBackup takes a backup for download: snapshot, compress to a
// finished file in scratchDir, read it back and verify it. The caller removes
// the file. Nothing is held in memory.
func (s *Server) buildManualBackup(r *http.Request, scratchDir string) (path string, size int64, err error) {
	ctx := r.Context()
	ticket, err := s.backupPreflight(scratchDir, scratchDir)
	if err != nil {
		return "", 0, err
	}
	// The finished file stays on disk while it is sent, and then shows in the
	// volume's free space itself; the reservation covers only the build.
	defer ticket.release()
	tmp, fp, raw, err := s.writeBackupArchive(ctx, scratchDir, scratchDir, "marbor-backup-manual-*.tmp", ticket)
	if err != nil {
		return "", 0, err
	}
	fail := func(e error) (string, int64, error) {
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("admin: could not remove the unfinished manual backup %s: %v", filepath.Base(tmp), rmErr)
		}
		return "", 0, e
	}
	if err := s.verifyArchive(ctx, tmp, fp, raw); err != nil {
		return fail(err)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return fail(newBackupFailure("could not read the finished backup", err))
	}
	return tmp, fi.Size(), nil
}

// handleBackupNow takes an on-demand backup and sends it back gzip compressed
// as a download - the manual counterpart to the scheduled job.
//
// The backup is built and verified first, while it holds the backup lock; the
// lock is released before a single byte is sent, so a slow or stalled download
// can never block the scheduled job. The result is recorded as soon as the
// backup is finished and verified: whether the requester then receives it is a
// separate matter (it only gets logged). The response carries an exact
// Content-Length, so a transfer cut short is visible to the client as an
// incomplete download rather than a valid-looking short file.
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	if s.housekeeping.Load() {
		respondHousekeeping(w)
		return
	}
	if s.backupsStopping.Load() {
		writeJSONError(w, http.StatusServiceUnavailable, "marbor is shutting down")
		return
	}
	if !s.acquireManualSlot() {
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusTooManyRequests, fmt.Sprintf("too many on-demand backups are in progress (at most %d at once, counting the download that follows each) - try again when one finishes", maxManualBackups))
		return
	}
	defer s.releaseManualSlot()
	if !s.tryRun() {
		writeJSONError(w, http.StatusConflict, "a backup is already running - try again when it finishes")
		return
	}
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			s.endRun()
		}
	}
	defer unlock()

	dir := s.currentTargetDir()
	statusDir := s.statusDirFor(dir)
	scratchDir, where := s.manualScratchDir(dir)
	log.Printf("admin: on-demand backup staged in %s", where)

	dc := s.newDeadlineControl(w)
	dc.clearRead()
	dc.clearWrite() // the build sends nothing; the transfer sets its own idle deadline

	path, size, err := s.buildManualBackup(r, scratchDir)
	var f *os.File
	if err == nil {
		// Reopen the finished file before anything is recorded: a backup whose file
		// cannot be opened for sending is not a success.
		var oerr error
		f, _, oerr = openBackupFile(path)
		if oerr != nil {
			err = newBackupFailure("could not open the finished backup", oerr)
		}
	}
	if err != nil && path != "" {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("admin: could not remove the on-demand backup file %s: %v", filepath.Base(path), rmErr)
		}
	}
	// A backup that was only deferred (another operation had the disk) did not run;
	// it is neither a success nor a failure and leaves the record alone.
	if !errors.Is(err, errBackupDeferred) {
		s.recordBackupOutcome(statusDir, backupSourceManual, err)
	}
	if err != nil {
		unlock()
		respondBackupBuildError(w, r, err)
		return
	}
	defer func() {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("admin: could not remove the on-demand backup file %s: %v", filepath.Base(path), rmErr)
		}
	}()
	unlock()
	defer f.Close()
	setDownloadHeaders(w.Header(), "application/gzip", backupFilename(time.Now()))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	sent, err := copyAll(idleWriter{ResponseWriter: w, dc: dc}, f)
	if err != nil || sent != size {
		// The backup itself is complete and recorded; only this transfer failed,
		// and the operator who asked for the file needs to be able to see that.
		log.Printf("admin: on-demand backup download was interrupted after %d of %d bytes: %v", sent, size, err)
		s.noteManualTransfer(statusDir, fmt.Sprintf("the backup was made but its download was interrupted after %s of %s, so no complete file was received", formatBytes(uint64(sent)), formatBytes(uint64(size))))
		return
	}
	s.logSystemChange(r, "manual_backup", "global", "Downloaded an on-demand marbor.db backup")
}

// handleDeleteBackup removes one kept backup file by name (admin only). The name
// must be a bare marbor-backup name that exists as a regular file in the backup
// directory - never a path. A backup that a restore or a download is reading is
// refused with 409. The deletion is written to the system audit log.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || filepath.Base(name) != name || !backupFilenameRE.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid backup filename")
		return
	}
	dir := s.currentTargetDir()
	if dir == "" {
		writeJSONError(w, http.StatusBadRequest, "no backup target directory configured")
		return
	}
	if s.housekeeping.Load() {
		respondHousekeeping(w)
		return
	}
	err := s.deleteBackup(dir, name)
	switch {
	case err == nil:
		s.logSystemChange(r, "delete_backup", "global", "Deleted backup "+name)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errNotRegularBackup):
		writeJSONError(w, http.StatusNotFound, "backup file not found")
	case errors.Is(err, errBackupInUse):
		writeJSONError(w, http.StatusConflict, "this backup is being downloaded or restored right now - try again when that finishes")
	default:
		writeCorrelatedError(w, r, http.StatusInternalServerError, "could not delete the backup file", err)
	}
}

// respondBackupBuildError answers a failed on-demand build. A cancelled request
// gets nothing (nobody is listening).
func respondBackupBuildError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *backupRefusedError
	var failure *backupFailure
	switch {
	case errors.Is(err, errBackupDeferred):
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusConflict, "another backup, upload or restore check is using the disk - try again when it finishes")
	case r.Context().Err() != nil:
		log.Printf("admin: on-demand backup abandoned: the requester disconnected")
	case errors.As(err, &refused):
		writeJSONError(w, http.StatusInsufficientStorage, refused.msg)
	case errors.As(err, &failure):
		writeJSONError(w, http.StatusInternalServerError, failure.public)
	default:
		writeServerError(w, r, err)
	}
}
