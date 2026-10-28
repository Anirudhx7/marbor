package admin

// backup_status.go - the persisted outcome of scheduled and manual backups.
//
// Scheduled and manual runs keep separate records: a manual download must not
// reset the scheduled run's retry backoff, hide a scheduled failure, or be
// mistaken for the scheduled success that drives the schedule. Every record
// carries the time of its own last attempt and of its own last success.
//
// Error text stored here and served by the API is always a fixed sentence plus
// a reference (see backupFailure): never an absolute path or an OS error. The
// full detail is in the server log under the same reference.
//
// The record is a small file next to the backups (or next to the database when
// the backup directory is missing or unset). It is written outside backupMu so
// a slow disk never stalls the lock that every status read takes; writes are
// ordered by a generation counter so an older snapshot never overwrites a newer
// one. The last restore result is separate (see backup_restorestate.go) because
// it describes the database, not a backup directory.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// backupOutcome is what one source (scheduled or manual) last did.
type backupOutcome struct {
	Result    string    `json:"result,omitempty"` // success, failed, refused, cancelled
	Error     string    `json:"error,omitempty"`
	Warning   string    `json:"warning,omitempty"` // a success that needs a look, e.g. a directory that could not be flushed
	Note      string    `json:"note,omitempty"`    // what happened after the backup was made, e.g. a download that was cut short
	AttemptAt time.Time `json:"attempt_at,omitzero"`
	SuccessAt time.Time `json:"success_at,omitzero"`
}

// backupStatusFile is the persisted form of every backup outcome.
type backupStatusFile struct {
	Scheduled        backupOutcome `json:"scheduled"`
	Manual           backupOutcome `json:"manual"`
	PruneError       string        `json:"prune_error,omitempty"`        // the scheduled pool
	UploadPruneError string        `json:"upload_prune_error,omitempty"` // the uploads pool
	LeftoverFiles    int           `json:"leftover_files,omitempty"`
	DirUnreadable    bool          `json:"dir_unreadable,omitempty"` // the last leftover scan could not read the directory
}

// backupOutcomeInfo is one source's outcome in the API.
type backupOutcomeInfo struct {
	Result    string `json:"result"`
	AttemptAt string `json:"attempt_at,omitempty"`
	SuccessAt string `json:"success_at,omitempty"`
	Error     string `json:"error,omitempty"`
	Warning   string `json:"warning,omitempty"`
	Note      string `json:"note,omitempty"`
}

// backupRestoreInfo is the last restore in the API.
type backupRestoreInfo struct {
	Result string `json:"result"`
	At     string `json:"at,omitempty"`
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// backupStatusInfo is the "status" object of GET /admin/backup/list. The
// last_* fields describe the most recent attempt of either source (and the most
// recent success of either), for clients that show one line; scheduled and
// manual give each source's own record.
type backupStatusInfo struct {
	LastResult    string `json:"last_result"` // "", success, failed, refused, cancelled
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastSource    string `json:"last_source,omitempty"`

	Scheduled backupOutcomeInfo `json:"scheduled"`
	Manual    backupOutcomeInfo `json:"manual"`

	Running          bool   `json:"running"`
	DirExists        bool   `json:"dir_exists"`
	DirWarning       string `json:"dir_warning,omitempty"`
	Notice           string `json:"notice,omitempty"`
	StatusNotSaved   string `json:"status_not_saved,omitempty"`
	PruneError       string `json:"prune_error,omitempty"`
	UploadPruneError string `json:"upload_prune_error,omitempty"`
	LeftoverFiles    int    `json:"leftover_files,omitempty"`
	DirScanError     string `json:"dir_scan_error,omitempty"`
	// RunningSince is when the backup now running started, so a client can tell
	// a long backup from a stuck one.
	RunningSince string `json:"running_since,omitempty"`
	// ScheduleEnabled and ScheduleIntervalHours say what the scheduler is set to
	// do, so a client can tell an overdue scheduled backup from an off schedule.
	ScheduleEnabled       bool               `json:"schedule_enabled"`
	ScheduleIntervalHours int                `json:"schedule_interval_hours"`
	LastRestore           *backupRestoreInfo `json:"last_restore,omitempty"`
}

// publicBackupError turns any error into the result keyword and operator-safe
// text to store and serve. Anything that is not already a fixed, safe message
// is logged in full under a fresh reference and replaced by a generic one.
func publicBackupError(err error) (result, text string) {
	var refused *backupRefusedError
	var failure *backupFailure
	switch {
	case err == nil:
		return backupResultSuccess, ""
	case errors.As(err, &refused):
		return backupResultRefused, refused.msg
	case errors.Is(err, context.Canceled):
		return backupResultCancelled, "the backup was cancelled before it finished"
	case errors.As(err, &failure):
		return backupResultFailed, failure.public
	default:
		ref := newCorrelationID()
		log.Printf("admin: backup failed (ref %s): %v", ref, err)
		return backupResultFailed, fmt.Sprintf("backup failed (ref %s)", ref)
	}
}

// statusDirFor is where backup status is kept: next to the backups, or - when no
// backup directory is configured - next to the database, so even "no target
// directory" is a failure that survives a restart.
func (s *Server) statusDirFor(targetDir string) string {
	if targetDir != "" {
		return targetDir
	}
	return s.dirOfDB()
}

func formatBackupTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// statusRead is what reading the status files of one directory found. It is
// produced without any lock held (the files may be on a slow or dead mount) and
// installed under backupMu.
type statusRead struct {
	st   backupStatusFile
	ok   bool
	note string
}

// maxStatusFileBytes bounds what is read as a status file; a real one is a few
// hundred bytes.
const maxStatusFileBytes = 1 << 20

var errStatusTooLarge = errors.New("status file is too large")

// readStatusBytes reads path only if it is a regular file (a symlink or device
// is refused, see openBackupFile) of at most maxStatusFileBytes.
func readStatusBytes(path string) ([]byte, error) {
	f, fi, err := openBackupFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > maxStatusFileBytes {
		return nil, errStatusTooLarge
	}
	return io.ReadAll(io.LimitReader(f, maxStatusFileBytes+1))
}

// readStatusFile loads one status file; ok is false when there is none. It
// touches no shared state.
func readStatusFile(dir string) (r statusRead) {
	data, err := readStatusBytes(filepath.Join(dir, backupStatusFileName))
	switch {
	case errors.Is(err, errNotRegularBackup) || errors.Is(err, errStatusTooLarge):
		log.Printf("admin: ignoring the backup status file: %v", err)
		r.note = "the saved backup status was not a plain file of the expected size and was ignored"
	case err == nil:
		if jerr := json.Unmarshal(data, &r.st); jerr != nil {
			log.Printf("admin: ignoring unreadable backup status file: %v", jerr)
			return statusRead{note: "the saved backup status was unreadable and was ignored"}
		}
		r.ok = true
	case !errors.Is(err, os.ErrNotExist):
		log.Printf("admin: could not read the backup status file: %v", err)
		r.note = "the saved backup status could not be read"
	}
	return r
}

// readStatusFor reads the status of dir, falling back to the copy kept next to
// the database. Lock-free.
func (s *Server) readStatusFor(dir string) (r statusRead) {
	if dir == "" {
		return r
	}
	r = readStatusFile(dir)
	if !r.ok {
		if dbDir := s.dirOfDB(); dbDir != "" && dbDir != dir {
			if r2 := readStatusFile(dbDir); r2.ok {
				return r2
			}
		}
	}
	return r
}

// prepareBackupStatus makes sure the status of dir is loaded, reading the files
// before taking backupMu so a slow disk never stalls the lock every status read
// takes. Every entry point that is about to take backupMu and call
// ensureBackupStatusLoaded calls this first; ensure then finds it loaded.
func (s *Server) prepareBackupStatus(dir string) {
	s.backupMu.Lock()
	loaded := s.backupStatusLoaded && s.backupStatusDir == dir
	s.backupMu.Unlock()
	if loaded {
		return
	}
	r := s.readStatusFor(dir)
	s.backupMu.Lock()
	s.installBackupStatus(dir, &r)
	s.backupMu.Unlock()
}

// ensureBackupStatusLoaded loads the status file of dir the first time it is
// needed and again whenever the configured directory changes, so the dashboard
// never shows another directory's history. When the directory changes a notice
// says so (cleared by the next successful backup). When dir has no status file
// the copy kept next to the database, written while dir was missing, is used.
// The caller holds backupMu.
func (s *Server) ensureBackupStatusLoaded(dir string) {
	s.installBackupStatus(dir, nil)
}

// installBackupStatus is ensureBackupStatusLoaded with the files already read
// (pre != nil); with nil it reads them itself, under the lock, which only
// happens when the directory changed between prepareBackupStatus and here.
func (s *Server) installBackupStatus(dir string, pre *statusRead) {
	if s.backupStatusLoaded && s.backupStatusDir == dir {
		return
	}
	first := !s.backupStatusLoaded
	prevDir := s.backupStatusDir
	prev := s.backupState
	s.backupStatusLoaded = true
	s.backupStatusDir = dir
	s.backupState = backupStatusFile{}
	s.backupPersistNote = ""
	if !first && prevDir != dir {
		s.backupNotice = "the backup directory changed, so the history shown here starts from the change; backups made earlier stay in the previous directory"
	}
	if dir != "" {
		if pre == nil {
			r := s.readStatusFor(dir)
			pre = &r
		}
		if pre.ok {
			s.backupState = pre.st
		}
		if pre.note != "" {
			s.backupPersistNote = pre.note
		}
	}
	if first && s.backupState.Scheduled.SuccessAt.IsZero() {
		// A scheduled success time seeded from an older release's setting.
		s.backupState.Scheduled.SuccessAt = prev.Scheduled.SuccessAt
	}
}

func (s *Server) setPersistNote(note string) {
	s.backupMu.Lock()
	s.backupPersistNote = note
	s.backupMu.Unlock()
}

// persistBackupStatus saves the current status. The snapshot is taken under
// backupMu; the write happens outside it, serialized by backupWriteMu, and a
// snapshot older than one already written is dropped. A failure is logged and
// remembered in backupPersistNote so the dashboard can say the status will not
// survive a restart; it never fails the backup itself. The caller must NOT hold
// backupMu.
func (s *Server) persistBackupStatus(dir string) {
	s.backupMu.Lock()
	data, merr := json.Marshal(s.backupState)
	s.backupStateGen++
	gen := s.backupStateGen
	s.backupMu.Unlock()
	if merr != nil {
		log.Printf("admin: could not save the backup status: %v", merr)
		s.setPersistNote("the backup status could not be saved (see the marbor log)")
		return
	}

	s.backupWriteMu.Lock()
	defer s.backupWriteMu.Unlock()
	if gen < s.backupWrittenGen {
		return // a newer snapshot has been written already
	}
	s.backupWrittenGen = gen
	s.setPersistNote(s.writeStatusFile(dir, data))
}

// writeStatusFile writes data into dir, or - when dir is missing - next to the
// database, and returns the note to show ("" when it is saved where expected).
func (s *Server) writeStatusFile(dir string, data []byte) string {
	const unsaved = "the backup status could not be saved (see the marbor log)"
	target, fellBack := dir, false
	if target != "" {
		if _, err := os.Stat(target); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Printf("admin: could not save the backup status: %v", err)
				return unsaved
			}
			target = ""
		}
	}
	if target == "" {
		target = s.dirOfDB()
		fellBack = true
		if target == "" {
			return "no directory is available to save the backup status; it is kept in memory only"
		}
	}
	dirErr, err := writeFileAtomic(filepath.Join(target, backupStatusFileName), data)
	if err != nil {
		log.Printf("admin: could not save the backup status: %v", err)
		return unsaved
	}
	if dirErr != nil {
		log.Printf("admin: could not flush the directory after saving the backup status: %v", dirErr)
	}
	if fellBack {
		if dir != "" {
			return "the backup directory does not exist yet; the status is saved next to the database until it does"
		}
		return ""
	}
	if dbDir := s.dirOfDB(); dbDir != "" && !sameDirectory(dbDir, target) {
		removeQuiet(filepath.Join(dbDir, backupStatusFileName))
	}
	return ""
}

// sameDirectory reports whether a and b are one directory on disk, however they
// are spelled (a symlink, a different case on Windows, a trailing dot). Two
// paths that cannot both be inspected are treated as the same directory, so the
// caller never deletes on a guess.
func sameDirectory(a, b string) bool {
	if a == b {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr != nil || berr != nil {
		return true
	}
	return os.SameFile(ai, bi)
}

// recordBackupOutcome stores the result of one attempt of the given source in
// memory and in the status file. err == nil is a success; a *backupRefusedError
// is a refusal for lack of disk space; a cancelled context is a cancellation;
// anything else is a failure.
func (s *Server) recordBackupOutcome(statusDir, source string, err error) {
	s.recordBackupOutcomeWarn(statusDir, source, err, "")
}

// recordBackupOutcomeWarn is recordBackupOutcome for a success that carries a
// warning (the backup is kept, but something around it needs a look).
func (s *Server) recordBackupOutcomeWarn(statusDir, source string, err error, warning string) {
	result, text := publicBackupError(err)
	if result == backupResultCancelled {
		switch {
		case s.backupsStopping.Load() && source == backupSourceScheduled:
			text = "the scheduled backup was interrupted because marbor was shutting down; it runs again after the restart"
		case s.backupsStopping.Load():
			text = "the backup was interrupted because marbor was shutting down"
		case source == backupSourceScheduled:
			text = "the scheduled backup was cancelled before it finished"
		default:
			text = "the backup was cancelled because the requester went away"
		}
	}
	now := time.Now()
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	s.ensureBackupStatusLoaded(statusDir)
	o := &s.backupState.Manual
	if source == backupSourceScheduled {
		o = &s.backupState.Scheduled
	}
	o.Result = result
	o.Error = text
	o.Warning = ""
	o.Note = ""
	o.AttemptAt = now
	if result == backupResultSuccess {
		o.SuccessAt = now
		o.Warning = warning
		s.backupNotice = ""
	}
	s.backupMu.Unlock()
	s.persistBackupStatus(statusDir)
}

// noteManualTransfer records what happened to an on-demand backup after it was
// made and recorded: a download that was cut short does not undo the backup,
// but the operator who clicked the button needs to know they have no file.
func (s *Server) noteManualTransfer(statusDir, note string) {
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	s.ensureBackupStatusLoaded(statusDir)
	s.backupState.Manual.Note = note
	s.backupMu.Unlock()
	s.persistBackupStatus(statusDir)
}

// recordPruneResult remembers whether the last retention pass of a pool could do
// what it meant to, so a disk that keeps filling up is visible. problem == ""
// clears the pool's earlier error.
func (s *Server) recordPruneResult(statusDir string, uploaded bool, problem string) {
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	s.ensureBackupStatusLoaded(statusDir)
	field := &s.backupState.PruneError
	if uploaded {
		field = &s.backupState.UploadPruneError
	}
	if *field == problem {
		s.backupMu.Unlock()
		return
	}
	*field = problem
	s.backupMu.Unlock()
	s.persistBackupStatus(statusDir)
}

// recordLeftovers stores how many leftover files the last scan could not remove,
// and whether the directory could not be read at all (a different state: the scan
// says nothing about leftovers then).
func (s *Server) recordLeftovers(statusDir string, stuck int, unreadable bool) {
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	s.ensureBackupStatusLoaded(statusDir)
	if s.backupState.LeftoverFiles == stuck && s.backupState.DirUnreadable == unreadable {
		s.backupMu.Unlock()
		return
	}
	s.backupState.LeftoverFiles = stuck
	s.backupState.DirUnreadable = unreadable
	s.backupMu.Unlock()
	s.persistBackupStatus(statusDir)
}

// noteScheduledDeferred records, in memory only, that a due scheduled backup is
// waiting for the disk or the directory. It is not a failure: no attempt time is
// set, so no backoff applies, and the next real outcome replaces the note.
func (s *Server) noteScheduledDeferred(statusDir, why string) {
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	s.ensureBackupStatusLoaded(statusDir)
	s.backupState.Scheduled.Note = "deferred: " + why + "; retrying shortly"
	s.backupMu.Unlock()
}

// scheduleState returns what the scheduler needs: the last scheduled success
// and the last scheduled attempt with its result.
func (s *Server) scheduleState(statusDir string) (lastSuccess, lastAttempt time.Time, lastResult string) {
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	s.ensureBackupStatusLoaded(statusDir)
	o := s.backupState.Scheduled
	return o.SuccessAt, o.AttemptAt, o.Result
}

func outcomeInfo(o backupOutcome) backupOutcomeInfo {
	return backupOutcomeInfo{
		Result:    o.Result,
		AttemptAt: formatBackupTime(o.AttemptAt),
		SuccessAt: formatBackupTime(o.SuccessAt),
		Error:     o.Error,
		Warning:   o.Warning,
		Note:      o.Note,
	}
}

// latestOutcome returns the more recent attempt of the two sources.
func latestOutcome(st backupStatusFile) (o backupOutcome, source string) {
	if st.Manual.AttemptAt.After(st.Scheduled.AttemptAt) {
		return st.Manual, backupSourceManual
	}
	if st.Scheduled.AttemptAt.IsZero() {
		return backupOutcome{}, ""
	}
	return st.Scheduled, backupSourceScheduled
}

func latestSuccess(st backupStatusFile) time.Time {
	if st.Manual.SuccessAt.After(st.Scheduled.SuccessAt) {
		return st.Manual.SuccessAt
	}
	return st.Scheduled.SuccessAt
}

// backupStatusSnapshot builds the "status" object for the list endpoint.
func (s *Server) backupStatusSnapshot(targetDir string) backupStatusInfo {
	statusDir := s.statusDirFor(targetDir)
	s.prepareBackupStatus(statusDir)
	s.backupMu.Lock()
	s.ensureBackupStatusLoaded(statusDir)
	st := s.backupState
	note := s.backupPersistNote
	notice := s.backupNotice
	restore := s.lastRestore
	s.backupMu.Unlock()

	last, source := latestOutcome(st)
	info := backupStatusInfo{
		LastResult:       last.Result,
		LastAttemptAt:    formatBackupTime(last.AttemptAt),
		LastSuccessAt:    formatBackupTime(latestSuccess(st)),
		LastError:        last.Error,
		LastSource:       source,
		Scheduled:        outcomeInfo(st.Scheduled),
		Manual:           outcomeInfo(st.Manual),
		Notice:           notice,
		StatusNotSaved:   note,
		PruneError:       st.PruneError,
		UploadPruneError: st.UploadPruneError,
		LeftoverFiles:    st.LeftoverFiles + int(s.scratchLeftovers.Load()),
	}
	if st.DirUnreadable {
		info.DirScanError = "the backup directory could not be read, so it was not checked for leftover files"
	}
	if restore != nil {
		info.LastRestore = &backupRestoreInfo{Result: restore.Result, At: formatBackupTime(restore.At), Error: restore.Error, Note: restore.Note, Ref: restore.Ref}
	}
	info.Running = s.backupRunning.Load()
	if since := s.backupRunSince.Load(); info.Running && since > 0 {
		info.RunningSince = time.Unix(since, 0).UTC().Format(time.RFC3339)
	}
	s.mu.RLock()
	info.ScheduleEnabled = s.cfg.Backup.Enabled
	info.ScheduleIntervalHours = s.cfg.Backup.IntervalHours
	s.mu.RUnlock()
	info.DirExists, info.DirWarning = backupDirState(targetDir)
	return info
}

// overlayBackupSettings fills the read-only last-backup fields of the settings
// response from the live status.
func (s *Server) overlayBackupSettings(cfg *config.BackupConfig) {
	dir := s.statusDirFor(cfg.TargetDir)
	s.prepareBackupStatus(dir)
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	s.ensureBackupStatusLoaded(dir)
	last, _ := latestOutcome(s.backupState)
	cfg.LastBackupAt = formatBackupTime(latestSuccess(s.backupState))
	cfg.LastBackupError = last.Error
	cfg.LastBackupResult = last.Result
	cfg.LastBackupAttemptAt = formatBackupTime(last.AttemptAt)
}

// currentTargetDir reads the live configured backup directory. It takes s.mu,
// so it must not be called with s.mu held.
func (s *Server) currentTargetDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Backup.TargetDir
}

// seedLegacyBackupStatus carries the last scheduled-success time that an older
// release stored in the settings table into the new status, so an upgrade does
// not run an immediate extra backup. The old error text is dropped: it could
// hold paths, and it described a state that no longer exists.
func (s *Server) seedLegacyBackupStatus() {
	v, err := s.st.GetSetting("backup_last_at")
	if err != nil || v == "" {
		return
	}
	t, perr := time.Parse(time.RFC3339, v)
	if perr != nil {
		return
	}
	s.backupMu.Lock()
	if s.backupState.Scheduled.SuccessAt.IsZero() {
		s.backupState.Scheduled.SuccessAt = t
	}
	s.backupMu.Unlock()
}
