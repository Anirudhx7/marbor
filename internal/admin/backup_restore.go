package admin

// backup_restore.go - the restore and upload handlers.
//
// Both validate a database-sized candidate file, which can take minutes, so
// each takes the single-flight lock backupHeavyMu (one upload or restore check
// at a time: two of them at once would each be allowed to expand to the whole
// free space), clears the connection's fixed timeouts for an idle timeout, and
// stops when the requester goes away.
//
// Every error shown to the client is a fixed sentence (or the fixed Reason of a
// store.BackupInvalidError) plus a request id; the detail, with paths and OS
// errors, goes to the server log under that id.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// errUploadTooSlow means an upload moved fewer bytes than the minimum rate, or
// ran past its total deadline: a client trickling bytes would otherwise hold the
// single upload slot (and a disk reservation) for as long as it likes.
var errUploadTooSlow = errors.New("upload is too slow")

// The upload limits. They are package variables so tests can shorten them; a test
// that also sets uploadClock must not run in parallel with another upload test,
// because the clock is shared.
var (
	uploadMinRate      int64 = 64 << 10         // bytes per second, measured over uploadRateWindow
	uploadRateWindow         = 30 * time.Second // length of one measurement window
	uploadBaseDeadline       = 5 * time.Minute  // allowed on top of size / uploadMinRate
	uploadMaxDeadline        = 2 * time.Hour    // the most any one upload may take, whatever its size
	uploadClock              = time.Now
)

// uploadDeadline is how much network read time an upload of size bytes (0 when
// unknown) is given: a base, plus the time the minimum rate needs for the size,
// never more than uploadMaxDeadline.
func uploadDeadline(size int64) time.Duration {
	expect := size
	if expect <= 0 {
		expect = maxUploadedBackupSize
	}
	d := uploadBaseDeadline + time.Duration(expect/max(uploadMinRate, 1))*time.Second
	return min(d, uploadMaxDeadline)
}

// rateGuard fails reads when the stream moves less than minRate bytes per second
// over a window, or when the total allowance is used up. It measures only the
// time spent inside the underlying read - the network - so a slow local disk, a
// pause while a hold grows, or any other work between reads is never charged to
// the client. A read that never returns is cut by the connection's own idle
// deadline.
type rateGuard struct {
	r       io.Reader
	now     func() time.Time
	minRate int64
	window  time.Duration
	limit   time.Duration // total network time allowed

	total    time.Duration
	winTime  time.Duration
	winBytes int64
}

func newRateGuard(r io.Reader, size int64) *rateGuard {
	return &rateGuard{r: r, now: uploadClock, minRate: uploadMinRate, window: uploadRateWindow, limit: uploadDeadline(size)}
}

func (g *rateGuard) Read(p []byte) (int, error) {
	start := g.now()
	n, err := g.r.Read(p)
	spent := g.now().Sub(start)
	g.total += spent
	if err != nil {
		return n, err
	}
	if g.total > g.limit {
		return n, errUploadTooSlow
	}
	g.winBytes += int64(n)
	g.winTime += spent
	if g.winTime >= g.window {
		if g.winBytes < int64(g.winTime.Seconds())*g.minRate {
			return n, errUploadTooSlow
		}
		g.winTime, g.winBytes = 0, 0
	}
	return n, nil
}

// meterReader asks the growing reservation for room before every read, so what is
// reserved always covers what has been (and is about to be) written. A refusal
// ends the upload with a 507; the size cap itself is enforced by the request body
// limit.
type meterReader struct {
	r    io.Reader
	hold *growingHold
	n    int64
}

func (m *meterReader) Read(p []byte) (int, error) {
	if err := m.hold.ensure(m.n + int64(len(p))); err != nil {
		return 0, err
	}
	n, err := m.r.Read(p)
	m.n += int64(n)
	return n, err
}

// localWriteError marks a failure of marbor's own disk write, as opposed to a
// failure reading the request body.
type localWriteError struct{ err error }

func (e *localWriteError) Error() string { return e.err.Error() }
func (e *localWriteError) Unwrap() error { return e.err }

type localWriter struct{ w io.Writer }

func (l localWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if err != nil {
		return n, &localWriteError{err}
	}
	return n, nil
}

// reserveForFile promises need bytes on dir's volume for a file about to be
// written, refusing unless the volume can hold them and still keep the safety
// margin free after what other writers have reserved. The caller releases the
// hold when the file is on disk (its size then shows in the free space).
func (s *Server) reserveForFile(dir string, need uint64) (*spaceHold, error) {
	hold, _, room, usable, err := s.reserveRoom(dir, need, true, "the file was not saved")
	if errors.Is(err, errNoRoom) {
		return nil, &backupRefusedError{msg: fmt.Sprintf("not enough free disk space for this file: need about %s, have %s (including a safety margin)",
			formatBytes(need+room.margin), formatBytes(usable+room.margin))}
	}
	return hold, err
}

// respondBackupFileError answers a failed validation, staging or upload. prefix
// introduces a store.BackupInvalidError's fixed reason ("backup file failed
// validation").
func respondBackupFileError(w http.ResponseWriter, r *http.Request, prefix string, err error) {
	var refused *backupRefusedError
	var invalid *store.BackupInvalidError
	var failure *backupFailure
	var noRoom *store.NoRoomError
	switch {
	case r.Context().Err() != nil:
		log.Printf("admin: backup request abandoned: the requester disconnected")
	case errors.Is(err, errBackupDeferred):
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusConflict, "another backup, upload or restore check is using the disk - try again when it finishes")
	case errors.As(err, &refused):
		writeJSONError(w, http.StatusInsufficientStorage, refused.msg)
	case errors.As(err, &noRoom):
		writeCorrelatedError(w, r, http.StatusInsufficientStorage,
			fmt.Sprintf("not enough free disk space: %s free, and at least %s must stay spare", formatBytes(noRoom.Free), formatBytes(noRoom.Needed)), err)
	case isDiskFull(err):
		writeCorrelatedError(w, r, http.StatusInsufficientStorage, "the disk filled up while the file was being written", err)
	case errors.As(err, &invalid):
		writeCorrelatedError(w, r, http.StatusUnprocessableEntity, prefix+": "+invalid.Reason, err)
	case errors.As(err, &failure):
		writeJSONError(w, http.StatusInternalServerError, failure.public)
	default:
		writeServerError(w, r, err)
	}
}

// invalidKind is the fixed complaint when a file's content and name disagree or
// the content is not a backup at all.
func invalidKind(name string, kind store.BackupKind) error {
	gz := strings.HasSuffix(name, ".gz")
	switch {
	case kind == store.BackupKindUnknown:
		return &store.BackupInvalidError{Reason: "not a SQLite database or a gzip of one"}
	case !gz && kind == store.BackupKindGzip:
		return &store.BackupInvalidError{Reason: "this is a gzip file but its name does not end in .db.gz"}
	case gz && kind != store.BackupKindGzip:
		return &store.BackupInvalidError{Reason: "the name ends in .gz but the file is not gzip data"}
	}
	return nil
}

// removeSQLiteSidecars removes the journal, write-ahead log and shared-memory
// files SQLite may have created next to path while the file was opened for
// validation. A failure is logged; the leftovers are swept at the next start.
func removeSQLiteSidecars(path string) {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		removeQuiet(path + suffix)
	}
}

// removeStaged removes a staged restore file and its sidecars.
func removeStaged(path string) {
	removeQuiet(path)
	removeSQLiteSidecars(path)
}

// stageRestore turns the opened backup src (size bytes) into a validated plain
// database file next to the live database, named for this restore's ref, and
// returns its path. The final swap is then a rename of that file, so the backup
// is decompressed and checked once, before marbor goes down, not twice. The
// staged file is created fresh with mode 0600 (never over an existing file), and
// any failure removes it.
func (s *Server) stageRestore(ctx context.Context, src *os.File, size int64, name, ref string) (staged string, err error) {
	stage := stagedRestorePath(s.st.DBPath(), ref)
	stageDir := filepath.Dir(stage)
	kind, err := store.SniffBackupReader(src)
	if err != nil {
		return "", newBackupFailure("could not read the backup file", err)
	}
	if kerr := invalidKind(name, kind); kerr != nil {
		return "", kerr
	}
	defer func() {
		if err != nil {
			removeStaged(stage)
		}
	}()

	if kind == store.BackupKindGzip {
		limit, hold, lerr := s.decompressLimit(stageDir, size)
		if lerr != nil {
			return "", lerr
		}
		defer hold.release()
		if _, _, derr := store.DecompressStream(ctx, io.NewSectionReader(src, 0, size), stage, limit, store.WithBudget(hold.ensure)); derr != nil {
			return "", derr
		}
	} else {
		hold, rerr := s.reserveForFile(stageDir, uint64(max(size, 0)))
		if rerr != nil {
			return "", rerr
		}
		defer hold.release()
		if cerr := copyToNewFile(ctx, io.NewSectionReader(src, 0, size), stage); cerr != nil {
			return "", cerr
		}
	}
	if verr := store.ValidateBackupFileContext(ctx, stage); verr != nil {
		return "", verr
	}
	removeSQLiteSidecars(stage)
	if serr := syncDir(stageDir); serr != nil {
		return "", newBackupFailure("could not flush the staged restore file to disk", serr)
	}
	return stage, nil
}

// copyToNewFile copies src into a brand-new 0600 file at path and syncs it.
func copyToNewFile(ctx context.Context, src io.Reader, path string) error {
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return newBackupFailure("could not create the staged restore file", err)
	}
	_, err = copyAll(localWriter{dst}, ctxReader{ctx, src})
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var lw *localWriteError
		if errors.As(err, &lw) && isDiskFull(err) {
			return &backupRefusedError{msg: "the disk filled up while the backup was being staged"}
		}
		return newBackupFailure("could not stage the backup file", err)
	}
	return nil
}

// respondRestoreSlotError answers a restore that could not claim the slot.
func respondRestoreSlotError(w http.ResponseWriter, err error) {
	if errors.Is(err, errRestoreClosed) {
		writeJSONError(w, http.StatusServiceUnavailable, "marbor is shutting down, so the restore was not started")
		return
	}
	writeJSONError(w, http.StatusConflict, "a restore is already in progress")
}

// handleRestoreBackup validates a one-click restore request and, if valid,
// stages the validated database next to the live one and hands the staged
// path off to main.go via restoreCh - it never touches the live database or
// exits the process itself (see SetRestoreChannel). Everything that can fail
// (reading, unpacking, space, validation) happens here, before the 202, so a
// bad request never takes marbor down. Only one restore is ever staged or
// pending (see beginRestore), and each stages into a file of its own.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if s.restoreCh == nil {
		writeJSONError(w, http.StatusNotImplemented, "restore is not available in this run mode")
		return
	}
	if s.st.DBPath() == "" {
		writeJSONError(w, http.StatusNotImplemented, "restore is not available: marbor is running with an in-memory database")
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// filepath.Base plus the strict name pattern rule out path traversal (../,
	// absolute paths, subdirectories): only a bare marbor-backup-<timestamp>
	// .db or .db.gz name already sitting directly in TargetDir is accepted.
	name := filepath.Base(req.Filename)
	if name != req.Filename || !backupFilenameRE.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid backup filename")
		return
	}
	targetDir := s.currentTargetDir()
	if targetDir == "" {
		writeJSONError(w, http.StatusBadRequest, "no backup target directory configured")
		return
	}

	// Unpacking and checking a large backup takes minutes: replace the fixed
	// timeouts with an idle timeout, and run one such check at a time.
	if s.housekeeping.Load() {
		respondHousekeeping(w)
		return
	}
	dc := s.newDeadlineControl(w)
	dc.clearRead()
	dc.clearWrite()
	if !s.tryHeavy() {
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusConflict, "another backup upload or restore check is already running - try again when it finishes")
		return
	}
	defer s.endHeavy()
	if err := s.beginRestore(); err != nil {
		respondRestoreSlotError(w, err)
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.abortRestore()
		}
	}()
	defer s.pinBackup(name)()

	src, fi, err := openBackupFile(filepath.Join(targetDir, name))
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errNotRegularBackup):
		writeJSONError(w, http.StatusNotFound, "backup file not found")
		return
	default:
		writeCorrelatedError(w, r, http.StatusInternalServerError, "could not open the backup file", err)
		return
	}
	defer src.Close()

	ref := newCorrelationID()
	staged, err := s.stageRestore(r.Context(), src, fi.Size(), name, ref)
	if err != nil {
		respondBackupFileError(w, r, "backup file failed validation", err)
		return
	}
	if err := s.handOffRestore(staged, ref, name); err != nil {
		removeStaged(staged)
		switch {
		case errors.Is(err, errRestoreClosed), errors.Is(err, errRestoreBusy):
			respondRestoreSlotError(w, err)
		default:
			respondBackupFileError(w, r, "backup file failed validation", err)
		}
		return
	}
	handedOff = true

	s.logSystemChange(r, "restore_backup", "global", fmt.Sprintf("Restore requested from %s - marbor restarting (ref %s)", name, ref))
	dc.idleWrite()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]any{"restarting": true, "file": name, "ref": ref}); err != nil {
		log.Printf("admin: could not write the restore response: %v", err)
	}
}

// handleAcknowledgeRestore clears the shown result of the last restore. The
// client names the result it showed with ?ref= ("-" for one without a reference);
// a result that is not that one is left alone (409), so one operator cannot
// dismiss a result another has not seen. The clearing is shared: it clears the
// result for every operator and for the CLI, and is written to the audit log.
func (s *Server) handleAcknowledgeRestore(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref != "-" && !validRestoreRef(ref) {
		writeJSONError(w, http.StatusBadRequest, "ref must be the reference of the result being cleared, or - for a result without one")
		return
	}
	err := s.AcknowledgeRestoreResult(ref)
	switch {
	case err == nil:
		s.logSystemChange(r, "ack_restore_result", "global", "Cleared the last restore result (ref "+ref+")")
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errRestoreRefMismatch):
		writeJSONError(w, http.StatusConflict, "the restore result changed since it was shown; reload to see the current one")
	default:
		writeCorrelatedError(w, r, http.StatusInternalServerError, "could not clear the saved restore result", err)
	}
}

// checkUploadedBackup validates a staged upload and returns the canonical
// fingerprint and decompressed size of its content. A compressed upload is
// fully unpacked into a scratch file, under the size limit, for the check;
// every returned error is one respondBackupFileError understands.
func (s *Server) checkUploadedBackup(ctx context.Context, kind store.BackupKind, tmpPath, targetDir string, size int64) (fingerprint string, raw int64, err error) {
	switch kind {
	case store.BackupKindPlain:
		if err := store.ValidateBackupFileContext(ctx, tmpPath); err != nil {
			return "", 0, err
		}
		fp, err := store.FileFingerprint(ctx, tmpPath)
		if err != nil {
			if ctx.Err() != nil {
				return "", 0, ctx.Err()
			}
			return "", 0, newBackupFailure("could not read the uploaded file", err)
		}
		return fp, size, nil
	case store.BackupKindGzip:
		limit, hold, err := s.decompressLimit(targetDir, size)
		if err != nil {
			return "", 0, err
		}
		defer hold.release()
		sf, err := os.CreateTemp(targetDir, "marbor-backup-scratch-*.db")
		if err != nil {
			return "", 0, newBackupFailure("could not create a scratch file", err)
		}
		scratch := sf.Name()
		if cerr := sf.Close(); cerr != nil {
			s.removeScratch(scratch)
			return "", 0, newBackupFailure("could not create a scratch file", cerr)
		}
		defer s.removeScratch(scratch)
		fp, n, err := store.DecompressBackup(ctx, tmpPath, scratch, limit, store.WithBudget(hold.ensure))
		if err != nil {
			return "", 0, err
		}
		if err := store.ValidateBackupFileContext(ctx, scratch); err != nil {
			return "", 0, err
		}
		return fp, n, nil
	default:
		return "", 0, &store.BackupInvalidError{Reason: "not a SQLite database or a gzip of one"}
	}
}

// nextUploadPart advances mr to the part named "file".
func nextUploadPart(mr *multipart.Reader) (*multipart.Part, error) {
	for {
		p, err := mr.NextPart()
		if err != nil {
			return nil, err
		}
		if p.FormName() == "file" {
			return p, nil
		}
	}
}

// respondUploadReadError answers a failure while receiving the upload body.
func respondUploadReadError(w http.ResponseWriter, r *http.Request, err error) {
	var tooBigBody *http.MaxBytesError
	var refused *backupRefusedError
	var local *localWriteError
	switch {
	case r.Context().Err() != nil:
		log.Printf("admin: upload abandoned: the client disconnected")
	case errors.Is(err, errUploadTooSlow):
		writeCorrelatedError(w, r, http.StatusRequestTimeout, fmt.Sprintf("the upload was too slow and was stopped (it must move at least %s per second)", formatBytes(uint64(uploadMinRate))), err)
	case errors.As(err, &tooBigBody):
		writeCorrelatedError(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the upload is larger than the %s limit", formatBytes(uint64(maxUploadedBackupSize))), err)
	case errors.As(err, &refused):
		writeJSONError(w, http.StatusInsufficientStorage, refused.msg)
	case errors.As(err, &local) && isDiskFull(err):
		writeCorrelatedError(w, r, http.StatusInsufficientStorage, "the disk filled up while the upload was being saved", err)
	case errors.As(err, &local):
		writeServerError(w, r, err)
	default:
		writeCorrelatedError(w, r, http.StatusBadRequest, "the upload was interrupted or could not be read", err)
	}
}

// handleUploadBackup lets an operator attach a local backup (plain .db or
// gzip .db.gz, recognised by content, not by file name) and adds it to the same
// pool of restorable backups. The upload is streamed straight into a staging
// file inside TargetDir under a byte budget taken from the free space, then
// validated with the same checks a restore uses before it becomes a candidate;
// a compressed upload is fully decompressed under the size limit for that.
// Uploads are kept in a pool of their own (see pruneBackups), so with a full
// retention of both kinds the directory holds up to twice the retention count.
func (s *Server) handleUploadBackup(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	targetDir := s.cfg.Backup.TargetDir
	retentionCount := s.cfg.Backup.RetentionCount
	s.mu.RUnlock()
	if targetDir == "" {
		writeJSONError(w, http.StatusBadRequest, "no backup target directory configured")
		return
	}
	if s.housekeeping.Load() {
		respondHousekeeping(w)
		return
	}
	if s.backupsStopping.Load() {
		writeJSONError(w, http.StatusServiceUnavailable, "marbor is shutting down")
		return
	}
	ctx := r.Context()
	dc := s.newDeadlineControl(w)
	dc.clearRead()
	dc.clearWrite()
	if !s.tryHeavy() {
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusConflict, "another backup upload or restore check is already running - try again when it finishes")
		return
	}
	pruneAfter := false
	defer func() {
		s.endHeavy()
		if pruneAfter {
			// Retention runs after the upload slot is free, so reading the directory
			// back never holds up the next upload or restore check.
			s.goBackground("upload retention", func(bg context.Context) {
				s.runPrune(bg, targetDir, retentionCount, true)
			})
		}
	}()
	if err := ensureBackupDir(targetDir); err != nil {
		writeCorrelatedError(w, r, http.StatusInternalServerError, "could not create the backup directory", err)
		return
	}

	// The reservation starts at what the upload says it will be (or a plausible
	// size when it does not say) and grows, in steps, as bytes arrive - so a small
	// upload does not lock the whole free space away from everything else for the
	// length of the transfer. It never goes past the absolute upload limit, and a
	// request the volume cannot grant ends the upload.
	initial := uint64(64 << 20)
	if r.ContentLength > 0 {
		initial = uint64(r.ContentLength) + copyBufferBytes
	}
	hold, err := s.newGrowingHold(targetDir, initial, uint64(maxUploadedBackupSize), "the upload was being saved")
	if err != nil {
		respondBackupFileError(w, r, "the upload was refused", err)
		return
	}
	released := false
	releaseHold := func() {
		if !released {
			released = true
			hold.release()
		}
	}
	defer releaseHold()

	guarded := newRateGuard(idleReader{r: r.Body, dc: dc}, r.ContentLength)
	r.Body = http.MaxBytesReader(w, struct {
		io.Reader
		io.Closer
	}{guarded, r.Body}, maxUploadedBackupSize)
	mr, err := r.MultipartReader()
	if err != nil {
		writeCorrelatedError(w, r, http.StatusBadRequest, "invalid upload (not a multipart form)", err)
		return
	}
	part, err := nextUploadPart(mr)
	switch {
	case errors.Is(err, io.EOF):
		writeJSONError(w, http.StatusBadRequest, "missing file field")
		return
	case err != nil:
		respondUploadReadError(w, r, err)
		return
	}

	tmp, err := os.CreateTemp(targetDir, "marbor-backup-upload-*.tmp")
	if err != nil {
		writeCorrelatedError(w, r, http.StatusInternalServerError, "failed to stage the upload", err)
		return
	}
	tmpPath := tmp.Name()
	defer func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("admin: could not remove the staged upload %s: %v", filepath.Base(tmpPath), rmErr)
		}
	}()
	size, err := copyAll(localWriter{tmp}, ctxReader{ctx, &meterReader{r: part, hold: hold}})
	if err == nil {
		err = syncFile(tmp)
	}
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = &localWriteError{cerr}
	}
	if err != nil {
		respondUploadReadError(w, r, err)
		return
	}
	// The file is on disk now and shows in the free space; the validation below
	// reserves what it needs on its own.
	releaseHold()

	kind, err := store.SniffBackupFile(tmpPath)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	fingerprint, raw, err := s.checkUploadedBackup(ctx, kind, tmpPath, targetDir, size)
	if err != nil {
		respondBackupFileError(w, r, "not a valid marbor.db backup", err)
		return
	}

	// An upload whose content matches a kept backup (e.g. a file the operator
	// just downloaded) reuses that entry instead of adding a duplicate.
	if dup := s.findDuplicateBackup(ctx, targetDir, fingerprint, raw, true, false); dup != "" {
		s.logSystemChange(r, "upload_backup", "global", fmt.Sprintf("Uploaded backup matched existing %s - reused it instead of adding a duplicate", dup))
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"filename": dup, "duplicate": true}); err != nil {
			log.Printf("admin: could not write the upload response: %v", err)
		}
		return
	}

	ext := ".db"
	if kind == store.BackupKindGzip {
		ext = ".db.gz"
		if err := recordUploadFingerprint(tmpPath, fingerprint); err != nil {
			writeCorrelatedError(w, r, http.StatusInternalServerError, "could not finish saving the upload", err)
			return
		}
	}

	name, err := s.commitBackupFile(targetDir, tmpPath, ext, true)
	if name == "" {
		respondBackupFileError(w, r, "not a valid marbor.db backup", err)
		return
	}
	resp := map[string]any{"filename": name}
	if err != nil {
		// The file is in place; only something around it failed. The caller is told,
		// so the answer is not a bare success the operator has to take on faith.
		log.Printf("admin: uploaded backup %s saved with a warning: %v", name, err)
		resp["warning"] = commitWarning(err)
	}
	s.noteVerified(filepath.Join(targetDir, name), fingerprint, raw)

	// Uploads are pruned among themselves: they have their own retention budget
	// and never push scheduled backups out. The pass runs after this request.
	pruneAfter = true

	s.logSystemChange(r, "upload_backup", "global", fmt.Sprintf("Uploaded a custom backup file as %s", name))
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("admin: could not write the upload response: %v", err)
	}
}

func syncFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		return &localWriteError{err}
	}
	return nil
}

// recordUploadFingerprint makes the last gzip member of an uploaded file carry
// the fingerprint this process computed. An embedded one in the uploaded file
// is never trusted (the last member wins), but a tail that already matches is
// left alone so re-uploading a downloaded backup does not grow the file.
func recordUploadFingerprint(path, fingerprint string) error {
	if have, ok := store.ReadFingerprintTail(path); ok && have == fingerprint {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	err = store.AppendFingerprintTail(f, fingerprint)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
