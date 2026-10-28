package admin

// backup_restorestate.go - the hand-off of a staged restore to main, and the
// markers that carry its result across the restart.
//
// A restore is a small state machine under restoreMu: idle -> staging (the
// handler is building the staged file) -> handed off (main has the path and the
// process is going down), or closed (shutdown began, no new restore accepted).
// Only one restore is ever staged at a time, and every staged file has its own
// name (<db>.restoring-<ref>), so a second request can never overwrite or
// delete the file a pending restore will use.
//
// Two small marker files live next to the database, both written atomically
// (unique temp file, fsync, rename, directory fsync):
//
//	<db>.restore-pending  written before the 202 is sent; its presence at the
//	                      next start means the restore was accepted but its
//	                      outcome was never recorded
//	<db>.restore-outcome  written by the process that swapped the database
//
// The next start folds whichever it finds into <db>.restore-status, the result
// the dashboard shows until the operator acknowledges it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type restoreState int

const (
	restoreIdle restoreState = iota
	restoreStaging
	restoreHandedOff
	restoreClosed
)

var (
	errRestoreBusy   = errors.New("a restore is already in progress")
	errRestoreClosed = errors.New("marbor is shutting down")
)

// restoreOutcome is the result of a restore, written by the process that
// performed it (or synthesised by the next start) and shown until acknowledged.
type restoreOutcome struct {
	Ref    string    `json:"ref,omitempty"` // matches the "ref" in the server log
	Result string    `json:"result"`        // success, failed
	Error  string    `json:"error,omitempty"`
	Note   string    `json:"note,omitempty"` // a caveat on a success, e.g. a directory that could not be flushed
	At     time.Time `json:"at,omitzero"`
}

// restorePending is what is known about a restore that was accepted.
type restorePending struct {
	Ref    string    `json:"ref"`
	File   string    `json:"file"`
	Staged string    `json:"staged"` // base name of the staged file
	At     time.Time `json:"at"`
	// NotCarriedOut is set by the process that decided not to carry the restore
	// out and removed the staged file, so the next start can say so instead of
	// guessing from a missing file.
	NotCarriedOut bool `json:"not_carried_out,omitempty"`
}

// refRE is the shape of a restore reference (newCorrelationID). Anything else
// found in a marker file is not shown to the operator.
var refRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// validRestoreRef reports whether ref is a reference this server could have made.
func validRestoreRef(ref string) bool { return refRE.MatchString(ref) }

// maxMarkerText bounds any free text read back from a marker file.
const maxMarkerText = 300

// cleanMarkerText drops control characters and bounds the length of text read
// from a file an operator (or anything else with disk access) could have edited.
func cleanMarkerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxMarkerText {
		s = s[:maxMarkerText]
	}
	return s
}

// sanitizeOutcome makes a restore outcome read from disk safe to show.
func sanitizeOutcome(o *restoreOutcome) {
	if !validRestoreRef(o.Ref) {
		o.Ref = ""
	}
	if o.Result != backupResultSuccess {
		o.Result = backupResultFailed
	}
	o.Error = cleanMarkerText(o.Error)
	o.Note = cleanMarkerText(o.Note)
}

func restoreOutcomePath(dbPath string) string { return dbPath + ".restore-outcome" }
func restorePendingPath(dbPath string) string { return dbPath + ".restore-pending" }
func restoreStatusPath(dbPath string) string  { return dbPath + ".restore-status" }

// stagedRestoreName is the base name of the file a restore with this ref stages.
func stagedRestoreName(dbPath, ref string) string {
	return filepath.Base(dbPath) + ".restoring-" + ref
}

// stagedRestorePath is where the restore with this ref stages its database: next
// to the live database, so the final swap is a rename on one volume.
func stagedRestorePath(dbPath, ref string) string {
	return filepath.Join(filepath.Dir(dbPath), stagedRestoreName(dbPath, ref))
}

// isStagedRestoreName reports whether name is a staged restore file or one of
// its SQLite sidecars for the database whose base name is dbBase.
//
// The match is exact (base, ".restoring-", a 16-hex reference, optional sidecar
// suffix), so a file an operator keeps next to the database under a similar name
// is never removed by the start-up sweep.
func isStagedRestoreName(name, dbBase string) bool {
	rest, ok := strings.CutPrefix(name, dbBase+".restoring-")
	if !ok {
		return false
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if ref, found := strings.CutSuffix(rest, suffix); found && validRestoreRef(ref) {
			return true
		}
	}
	return false
}

// writeFileAtomic replaces path with data: a uniquely named temporary file in
// the same directory (created exclusively, mode 0600), fsync, rename, then a
// directory fsync. The directory fsync result is returned separately: when only
// that fails the file is in place but may not survive a power loss.
func writeFileAtomic(path string, data []byte) (dirSyncErr, err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "marbor-backup-marker-*.tmp")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("admin: could not remove the temporary file %s: %v", filepath.Base(tmp), rmErr)
		}
		return nil, werr
	}
	return syncDir(dir), nil
}

// beginRestore claims the single restore slot.
func (s *Server) beginRestore() error {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	switch s.restoreState {
	case restoreIdle:
		s.restoreState = restoreStaging
		return nil
	case restoreClosed:
		return errRestoreClosed
	default:
		return errRestoreBusy
	}
}

// abortRestore gives the slot back after a restore that did not reach main.
func (s *Server) abortRestore() {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	if s.restoreState == restoreStaging {
		s.restoreState = restoreIdle
	}
}

// handOffRestore records the pending marker and passes the staged path to main.
// The marker is durable before the channel send, so no 202 is ever sent for a
// restore whose existence the next start could not discover. It fails, leaving
// the staged file for the caller to remove, when shutdown began while staging.
//
// The marker is written without holding restoreMu: a slow disk must not stall
// CloseRestores, which main calls at shutdown. The state is checked before the
// write and again after it, under the lock; when shutdown began in between, the
// marker is removed and the caller removes the staged file.
func (s *Server) handOffRestore(staged, ref, file string) error {
	s.restoreMu.Lock()
	state := s.restoreState
	s.restoreMu.Unlock()
	if state != restoreStaging {
		return errRestoreClosed
	}
	dbPath := s.st.DBPath()
	info, statErr := os.Lstat(staged) // what was validated, for the final identity check
	if statErr != nil {
		return newBackupFailure("could not read the staged restore file", statErr)
	}
	data, err := json.Marshal(restorePending{Ref: ref, File: cleanMarkerText(file), Staged: filepath.Base(staged), At: time.Now()})
	if err != nil {
		return newBackupFailure("could not record the pending restore", err)
	}
	dirErr, err := writeFileAtomic(restorePendingPath(dbPath), data)
	if err != nil {
		return newBackupFailure("could not record the pending restore, so it was not started", err)
	}
	if dirErr != nil {
		log.Printf("admin: restore %s: could not flush the directory after writing the pending marker: %v", ref, dirErr)
	}
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	if s.restoreState != restoreStaging {
		removeQuiet(restorePendingPath(dbPath))
		return errRestoreClosed
	}
	select {
	case s.restoreCh <- staged:
	default:
		removeQuiet(restorePendingPath(dbPath))
		return errRestoreBusy
	}
	s.restoreState = restoreHandedOff
	s.restoreStaged = staged
	s.restoreStagedInfo = info
	return nil
}

// StagedRestoreUnchanged reports whether the file at path is still the one that
// was validated when the restore was accepted: same file, size and modification
// time. main uses it to skip a second full validation after the servers have
// drained (the file sits next to the live database, in a directory only marbor
// writes to; a full re-read of a multi-gigabyte file would hold the swap while
// nothing is serving). When identity cannot be proven, main validates in full.
func (s *Server) StagedRestoreUnchanged(path string) bool {
	s.restoreMu.Lock()
	want := s.restoreStagedInfo
	staged := s.restoreStaged
	s.restoreMu.Unlock()
	if want == nil || staged != path {
		return false
	}
	cur, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return os.SameFile(want, cur) && cur.Size() == want.Size() && cur.ModTime().Equal(want.ModTime())
}

// MarkRestoreNotCarriedOut records in the pending marker that the restore was
// abandoned and its staged file removed, so the next start can report it
// precisely. A missing marker is not an error.
func MarkRestoreNotCarriedOut(dbPath string) {
	p, ok := readPendingRestore(dbPath)
	if !ok {
		return
	}
	p.NotCarriedOut = true
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	if _, err := writeFileAtomic(restorePendingPath(dbPath), data); err != nil {
		log.Printf("admin: could not record that the restore was not carried out: %v", err)
	}
}

// CloseRestores stops accepting restores (main calls it as soon as shutdown
// starts) and returns the staged path of a restore that was handed off, so
// main performs it even when the signal that began the shutdown won the race
// against the channel. It returns "" when no restore is pending.
func (s *Server) CloseRestores() string {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	s.restoreState = restoreClosed
	return s.restoreStaged
}

func removeQuiet(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("admin: could not remove %s: %v", filepath.Base(path), err)
	}
}

// readPendingRestore returns the pending marker next to dbPath, if any.
func readPendingRestore(dbPath string) (restorePending, bool) {
	data, err := os.ReadFile(restorePendingPath(dbPath))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("admin: could not read the pending restore marker: %v", err)
		}
		return restorePending{}, false
	}
	var p restorePending
	if jerr := json.Unmarshal(data, &p); jerr != nil {
		log.Printf("admin: ignoring an unreadable pending restore marker: %v", jerr)
		return restorePending{}, false
	}
	if !validRestoreRef(p.Ref) {
		p.Ref = ""
	}
	// The staged name must be a bare file name of the kind this server creates
	// next to the database; anything else (a path, another prefix) is dropped, so
	// a edited marker can never point the start-up code at another file.
	if p.Staged != filepath.Base(p.Staged) || !strings.HasPrefix(p.Staged, filepath.Base(dbPath)+".restoring-") || strings.ContainsAny(p.Staged, `/\`) {
		p.Staged = ""
	}
	p.File = cleanMarkerText(p.File)
	return p, true
}

// WriteRestoreOutcome records, next to the database file, how a restore ended.
// The process exits right after a restore, so the next start reads this and
// shows the result in the dashboard. restoreErr nil means success; note is a
// caveat on a success. It returns the reference of the restore (taken from the
// pending marker) that the outcome text and the log both carry, so the operator
// can match them. The pending marker is removed once the outcome is durable.
func WriteRestoreOutcome(dbPath string, restoreErr error, note string) (ref string, err error) {
	pending, _ := readPendingRestore(dbPath)
	ref = pending.Ref
	o := restoreOutcome{Ref: ref, Result: backupResultSuccess, Note: note, At: time.Now()}
	if restoreErr != nil {
		o.Result = backupResultFailed
		o.Error = "the restore failed; the previous database is still in place"
		if ref != "" {
			o.Error += " (ref " + ref + ", see the marbor log)"
		} else {
			o.Error += " (see the marbor log)"
		}
	}
	data, err := json.Marshal(o)
	if err != nil {
		return ref, err
	}
	dirErr, err := writeFileAtomic(restoreOutcomePath(dbPath), data)
	if err != nil {
		return ref, err
	}
	if dirErr != nil {
		log.Printf("WARNING: could not flush the directory after recording the restore outcome: %v", dirErr)
	}
	removeQuiet(restorePendingPath(dbPath))
	return ref, nil
}

// loadLastRestore reads the persisted restore result into memory.
func (s *Server) loadLastRestore(dbPath string) {
	data, err := os.ReadFile(restoreStatusPath(dbPath))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("admin: could not read the saved restore result: %v", err)
		}
		return
	}
	var o restoreOutcome
	if jerr := json.Unmarshal(data, &o); jerr != nil || o.Result == "" {
		log.Printf("admin: ignoring an unreadable saved restore result: %v", jerr)
		return
	}
	sanitizeOutcome(&o)
	s.backupMu.Lock()
	s.lastRestore = &o
	s.backupMu.Unlock()
}

// absorbRestoreOutcome folds what the previous process left (an outcome marker,
// or only a pending marker when it died before recording one) into the saved
// restore result. A marker is removed only after the result was saved, so a
// failed save is retried at the next start instead of losing the answer. It
// must run before leftover staged files are removed: whether the staged file
// still exists is what tells "never carried out" from "unknown".
func (s *Server) absorbRestoreOutcome() {
	dbPath := s.st.DBPath()
	if dbPath == "" {
		return
	}
	s.loadLastRestore(dbPath)

	var o *restoreOutcome
	if data, err := os.ReadFile(restoreOutcomePath(dbPath)); err == nil {
		var parsed restoreOutcome
		if jerr := json.Unmarshal(data, &parsed); jerr != nil || parsed.Result == "" {
			log.Printf("admin: ignoring an unreadable restore outcome file: %v", jerr)
			removeQuiet(restoreOutcomePath(dbPath))
		} else {
			sanitizeOutcome(&parsed)
			o = &parsed
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("admin: could not read the restore outcome file: %v", err)
	}
	if o == nil {
		if p, ok := readPendingRestore(dbPath); ok {
			o = s.synthesizePendingOutcome(dbPath, p)
		}
	}
	if o == nil {
		return
	}
	log.Printf("admin: restore %s from the previous run: %s", orDash(o.Ref), o.Result)

	if err := saveRestoreStatus(dbPath, o); err != nil {
		log.Printf("admin: could not save the restore result; the marker is kept and read again at the next start: %v", err)
		s.backupMu.Lock()
		s.lastRestore = o
		s.backupMu.Unlock()
		return
	}
	s.backupMu.Lock()
	s.lastRestore = o
	s.backupMu.Unlock()
	removeQuiet(restoreOutcomePath(dbPath))
	removeQuiet(restorePendingPath(dbPath))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// synthesizePendingOutcome answers for a restore that was accepted but whose
// outcome was never written (the process was killed, or lost power). If the
// staged file is still there the swap never happened; if it is gone, the
// rename may or may not have completed and only the operator can tell.
func (s *Server) synthesizePendingOutcome(dbPath string, p restorePending) *restoreOutcome {
	statErr := errors.New("no staged file named")
	if p.Staged != "" {
		_, statErr = os.Lstat(filepath.Join(filepath.Dir(dbPath), p.Staged))
	}
	o := &restoreOutcome{Ref: p.Ref, Result: backupResultFailed, At: time.Now()}
	if p.NotCarriedOut {
		o.Error = "the restore was accepted but was not carried out; the previous database is still in place"
	} else if statErr == nil {
		o.Error = "the restore was accepted but marbor stopped before it was carried out; the previous database is still in place"
	} else {
		o.Error = "the restore was accepted but marbor stopped before recording whether it finished; check the data to see which database is in place"
	}
	if p.Ref != "" {
		o.Error += fmt.Sprintf(" (ref %s)", p.Ref)
	}
	return o
}

func saveRestoreStatus(dbPath string, o *restoreOutcome) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	dirErr, err := writeFileAtomic(restoreStatusPath(dbPath), data)
	if err != nil {
		return err
	}
	if dirErr != nil {
		log.Printf("admin: could not flush the directory after saving the restore result: %v", dirErr)
	}
	return nil
}

// errRestoreRefMismatch means the result being acknowledged is not the one the
// client showed (another result replaced it, or it was already cleared).
var errRestoreRefMismatch = errors.New("the restore result changed")

// AcknowledgeRestoreResult clears the shown result of the last restore, if its
// reference is ref ("-" stands for a result with no reference). The saved copy
// is removed first and the in-memory result cleared only after that worked: a
// failed removal leaves the result visible, so what is shown is what would come
// back at the next start. A client acknowledging a result it did not show gets
// errRestoreRefMismatch.
func (s *Server) AcknowledgeRestoreResult(ref string) error {
	s.backupMu.Lock()
	cur := s.lastRestore
	s.backupMu.Unlock()
	if cur == nil {
		return nil // already cleared: acknowledging twice is fine
	}
	if orDash(cur.Ref) != orDash(ref) {
		return errRestoreRefMismatch
	}
	if dbPath := s.st.DBPath(); dbPath != "" {
		if err := os.Remove(restoreStatusPath(dbPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.backupMu.Lock()
	if s.lastRestore == cur {
		s.lastRestore = nil
	}
	s.backupMu.Unlock()
	return nil
}

// cleanupStagedRestores removes every staged restore file (and its sidecars)
// that an earlier process left next to the database, and returns how many it
// could not remove.
func cleanupStagedRestores(dbPath string) (stuck int) {
	if dbPath == "" {
		return 0
	}
	dir, base := filepath.Dir(dbPath), filepath.Base(dbPath)
	entries, err := readDirTimeout(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("admin: could not scan for staged restore files: %v", err)
		}
		return 0
	}
	for _, e := range entries {
		if e.IsDir() || !isStagedRestoreName(e.Name(), base) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := fsTimeout(func() error { return os.Remove(path) }); err != nil {
			log.Printf("admin: could not remove staged restore file %s left by an earlier run: %v", e.Name(), err)
			stuck++
			continue
		}
		log.Printf("admin: removed staged restore file %s left by an earlier run", e.Name())
	}
	return stuck
}
