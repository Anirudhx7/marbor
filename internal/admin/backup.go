package admin

// backup.go - names, shared types and small helpers for the marbor.db backup
// feature. The pieces live in several files:
//
//	backup.go          names, types, directory and file helpers
//	backup_status.go   the persisted scheduled / manual / restore outcomes
//	backup_space.go    free-space checks and the decompression budget
//	backup_run.go      the scheduler, taking a backup, retention, verification
//	backup_http.go     list, download and on-demand backup handlers
//	backup_restore.go  restore and upload handlers
//	backup_restorestate.go  the single staged-restore state machine
//	backup_verify.go   read-back verification and its in-memory cache
//	backup_os_*.go     platform-specific disk and permission helpers
//
// New backups are gzip files (marbor-backup-<ts>.db.gz); older plain
// marbor-backup-<ts>.db files keep listing, downloading, restoring and pruning
// exactly as before. Everything that touches a database-sized file streams.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/diskspace"
)

const (
	// backupStatusFileName holds the backup outcomes next to the backups. It
	// lives outside marbor.db on purpose: a backup run must not write into the
	// database it is about to snapshot, or two backups of an otherwise
	// unchanged database would never match.
	backupStatusFileName = ".marbor-backup-status.json"

	// backupCheckInterval is how often StartBackupScheduler wakes up to check
	// whether a scheduled backup is due. It is intentionally much shorter than
	// the configurable IntervalHours (which can be changed live via Settings
	// without a restart) - each wake checks elapsed time since the last
	// successful backup against the current interval.
	backupCheckInterval = 15 * time.Minute

	// backupFailureBackoff is the minimum wait before a scheduled backup is
	// retried after a failed or refused attempt. Without it a run that cannot
	// finish would restart every check and keep hammering the disk.
	backupFailureBackoff = time.Hour

	// backupDeferRetry is how soon a scheduled run that was deferred (another
	// backup operation was using the disk, or the directory was being swept) is
	// tried again. A deferral is not a failure and starts no backoff.
	backupDeferRetry = time.Minute

	// backupSkipLogEvery limits how often a skipped scheduled run is logged.
	backupSkipLogEvery = time.Hour

	backupResultSuccess   = "success"
	backupResultFailed    = "failed"
	backupResultRefused   = "refused"
	backupResultCancelled = "cancelled"

	backupSourceScheduled = "scheduled"
	backupSourceManual    = "manual"

	// maxBackupNameCollisions bounds the search for a free file name when
	// several backups finish within the same second.
	maxBackupNameCollisions = 120

	integrityOK         = "ok"         // read in full and checked
	integrityUnverified = "unverified" // looks like a backup, not read in full yet
	integrityInvalid    = "invalid"    // failed a check
)

var (
	// Replaceable in tests.
	backupFreeBytes   = diskspace.Free
	backupTotalBytes  = func(dir string) (uint64, error) { u, err := diskspace.Stat(dir); return u.Total, err }
	backupSameVolume  = diskspace.SameVolume
	backupIdleTimeout = 2 * time.Minute

	// maxUploadedBackupSize caps a browser-uploaded backup file to 2 GiB of
	// request body (a compressed backup of a far larger database fits).
	maxUploadedBackupSize int64 = 2 << 30

	// maxDecompressedBackupBytes is the hard ceiling on how far a compressed
	// backup may expand during restore or upload validation (a gzip bomb
	// guard); the compressed size and the free space lower it further.
	maxDecompressedBackupBytes int64 = 1 << 40

	errBackupRefused = errors.New("backup refused")
	errBackupBusy    = errors.New("another backup is already running")

	// errBackupDeferred marks a scheduled run that was not started because
	// another backup operation held the disk or the backup directory was being
	// swept. It is neither a failure nor a refusal and is retried shortly.
	errBackupDeferred = errors.New("backup deferred")

	// backupFilenameRE matches the marbor-backup-<UTC timestamp>.db and .db.gz
	// names, with an optional -upload marker for files an operator uploaded.
	// Group 1 is the timestamp, group 2 the upload marker.
	backupFilenameRE = regexp.MustCompile(`^marbor-backup-(\d{8}-\d{6})(-upload)?\.db(\.gz)?$`)
)

// backupBaseName is marbor-backup-<UTC timestamp>, without an extension.
func backupBaseName(t time.Time) string {
	return fmt.Sprintf("marbor-backup-%s", t.UTC().Format("20060102-150405"))
}

// backupFilename returns the marbor-backup-<UTC timestamp>.db.gz name used by
// the scheduled job and the on-demand download.
func backupFilename(t time.Time) string { return backupBaseName(t) + ".db.gz" }

func backupSortKey(name string) string {
	if m := backupFilenameRE.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

func isUploadedBackupName(name string) bool {
	m := backupFilenameRE.FindStringSubmatch(name)
	return m != nil && m[2] != ""
}

// backupFileInfo is one entry in GET /admin/backup/list's response.
type backupFileInfo struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	Compressed bool      `json:"compressed"`
	Uploaded   bool      `json:"uploaded"`
	// Integrity is "ok" (read in full and checked by this process), "unverified"
	// (looks like a backup but has not been read in full) or "invalid".
	Integrity string `json:"integrity"`
}

// backupFailure is a failed backup step. Its Error text is safe to show to the
// operator: a fixed sentence plus a reference that the server log line (which
// carries the full detail, including paths and OS errors) repeats.
type backupFailure struct {
	public string
	err    error
}

func (e *backupFailure) Error() string { return e.public }
func (e *backupFailure) Unwrap() error { return e.err }

// newBackupFailure logs detail server-side under a fresh reference and returns
// an error that carries only the fixed message and that reference.
func newBackupFailure(public string, detail error) error {
	ref := newCorrelationID()
	log.Printf("admin: backup: %s (ref %s): %v", public, ref, detail)
	return &backupFailure{public: fmt.Sprintf("%s (ref %s)", public, ref), err: detail}
}

// backupRefusedError is a refusal for lack of disk space. Its message is built
// only from sizes and fixed volume descriptions, so it is safe to show to the
// operator as is.
type backupRefusedError struct{ msg string }

func (e *backupRefusedError) Error() string        { return e.msg }
func (e *backupRefusedError) Is(target error) bool { return target == errBackupRefused }

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---- directory helpers -----------------------------------------------------

// ensureBackupDir creates dir with mode 0700 when it does not exist. An
// existing directory is left exactly as it is. The 0700 mode and the warning
// below have no effect on Windows, where Go maps the permission bits onto the
// read-only attribute only; access there is governed by the folder's ACL.
func ensureBackupDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// backupDirState reports whether dir exists and anything worth telling the
// operator about it, without ever changing it. The warning never contains the
// directory path (the operator configured it and the dashboard shows it).
func backupDirState(dir string) (exists bool, warning string) {
	if dir == "" {
		return false, ""
	}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, ""
	case err != nil:
		log.Printf("admin: could not inspect the backup directory: %v", err)
		if errors.Is(err, os.ErrPermission) {
			return false, "the backup directory cannot be read: permission denied"
		}
		return false, "the backup directory cannot be inspected (see the marbor log)"
	case !fi.IsDir():
		return false, "the configured backup location is not a directory"
	}
	var warnings []string
	if w := dirOwnerWarning(fi); w != "" {
		warnings = append(warnings, w)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			warnings = append(warnings, fmt.Sprintf("the backup directory has mode %04o; backups contain API keys and session data, so restrict it to the service user (chmod 700)", perm))
		}
	}
	return true, strings.Join(warnings, "; ")
}

// dirOwnerWarning reports a backup directory owned by someone other than the
// service user.
func dirOwnerWarning(fi os.FileInfo) string {
	owner, ok := dirOwnerUID(fi)
	if !ok {
		return ""
	}
	return foreignOwnerWarning(owner, processUID())
}

// foreignOwnerWarning is the decision behind dirOwnerWarning. A directory owned
// by another user lets that user replace it with a symlink or swap backup files
// inside it, redirecting where marbor writes and deleting its own backup names.
// A root-owned directory is not reported, since only root can change it. The
// text carries no path (the dashboard already shows the configured directory).
func foreignOwnerWarning(owner, proc uint32) string {
	if owner == proc || owner == 0 {
		return ""
	}
	return fmt.Sprintf("the backup directory is owned by another user (uid %d), who could replace it or the backup files in it; use a directory owned by the service account", owner)
}

// syncDir flushes a directory's entries to disk so a rename or removal inside
// it survives a crash. Windows cannot open a directory for syncing, so it is a
// no-op there (NTFS journals metadata itself).
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// errNotRegularBackup means a backup name resolved to something that is not a
// plain file (a symlink, a directory, a device).
var errNotRegularBackup = errors.New("not a regular file")

// openBackupFile opens path for reading only if it is a regular file, closing
// the gap between checking a file and opening it: the path is Lstat'd (a
// symlink or any non-regular file is refused), opened without following
// symlinks where the platform supports it, and the opened descriptor is
// compared with the Lstat result, so a file swapped in between is detected.
func openBackupFile(path string) (*os.File, os.FileInfo, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, errNotRegularBackup
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, nil, err
	}
	fst, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fst.Mode().IsRegular() || !os.SameFile(lst, fst) {
		f.Close()
		return nil, nil, errNotRegularBackup
	}
	return f, fst, nil
}

// ---- response deadlines ------------------------------------------------------

// The admin listener runs with a short read and write timeout (it bounds slow
// clients on ordinary routes). A backup upload, download or restore legitimately
// takes minutes, so those handlers replace the fixed timeouts with an idle
// timeout: the connection may stay open for as long as bytes keep moving, and
// is cut after backupIdleTimeout without progress.

// deadlineControl sets connection deadlines through http.ResponseController.
// It logs once if the response writer chain cannot reach the connection.
type deadlineControl struct {
	rc     *http.ResponseController
	obs    *atomic.Pointer[func(op string, err error)]
	warned bool
}

func (s *Server) newDeadlineControl(w http.ResponseWriter) *deadlineControl {
	return &deadlineControl{rc: http.NewResponseController(w), obs: &s.deadlineObs}
}

// note reports a deadline change to the observer and logs the first failure of
// this request. http.ErrNotSupported means a wrapper between the handler and the
// connection does not pass deadline calls through: the listener's fixed timeouts
// then still apply and a long transfer is cut, so it is a warning like any other
// failure, not something to ignore.
func (d *deadlineControl) note(op string, err error) {
	if d.obs != nil {
		if obs := d.obs.Load(); obs != nil {
			(*obs)(op, err)
		}
	}
	if err != nil && !d.warned {
		d.warned = true
		log.Printf("admin: could not adjust a backup connection deadline (%s): %v", op, err)
	}
}

// SetDeadlineObserver installs a function that is told about every connection
// deadline change a backup transfer through this server makes and whether it
// worked. It exists so a test can prove, through the real middleware chain, that
// the deadlines reach the connection. Pass nil to remove it.
func (s *Server) SetDeadlineObserver(fn func(op string, err error)) {
	if fn == nil {
		s.deadlineObs.Store(nil)
		return
	}
	s.deadlineObs.Store(&fn)
}

func (d *deadlineControl) clearRead()  { d.note("clear-read", d.rc.SetReadDeadline(time.Time{})) }
func (d *deadlineControl) clearWrite() { d.note("clear-write", d.rc.SetWriteDeadline(time.Time{})) }
func (d *deadlineControl) idleRead() {
	d.note("idle-read", d.rc.SetReadDeadline(time.Now().Add(backupIdleTimeout)))
}
func (d *deadlineControl) idleWrite() {
	d.note("idle-write", d.rc.SetWriteDeadline(time.Now().Add(backupIdleTimeout)))
}

// idleWriter extends the write deadline before every write.
type idleWriter struct {
	http.ResponseWriter
	dc *deadlineControl
}

func (w idleWriter) Write(p []byte) (int, error) {
	w.dc.idleWrite()
	return w.ResponseWriter.Write(p)
}

func (w idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// idleReader extends the read deadline before every read.
type idleReader struct {
	r  io.Reader
	dc *deadlineControl
}

func (r idleReader) Read(p []byte) (int, error) {
	r.dc.idleRead()
	return r.r.Read(p)
}

// ---- small helpers -------------------------------------------------------------

// ctxReader fails reads once the request is gone, so a long copy stops.
type ctxReader struct {
	ctx interface{ Err() error }
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// dirOfDB returns the directory holding the live database, or "" when the
// store is in memory.
func (s *Server) dirOfDB() string {
	if p := s.st.DBPath(); p != "" {
		return filepath.Dir(p)
	}
	return ""
}

// leftoverNameRE matches exactly the temporary names marbor creates (os.CreateTemp
// with a random decimal suffix): the scratch snapshot and its SQLite sidecars,
// the upload, manual and scheduled write files, the atomic-marker temporaries
// and the hidden download-placeholder names. A file an operator happened to name
// marbor-backup-something.tmp does not match and is never touched.
var leftoverNameRE = regexp.MustCompile(`^(marbor-backup-scratch-\d+\.db(-journal|-wal|-shm)?|marbor-backup-(upload|manual|write|marker)-\d+\.tmp|marbor-backup-\d{8}-\d{6}(-upload)?\.db(\.gz)?\.tmp)$`)

func isBackupLeftover(name string) bool {
	return leftoverNameRE.MatchString(name)
}

// noStore marks a response as not cacheable: backup listings, statuses and
// restore answers describe a database that holds API keys and sessions.
func noStore(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

// respondHousekeeping answers a request that arrived while the backup directory
// is being swept for leftovers.
func respondHousekeeping(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	writeJSONError(w, http.StatusServiceUnavailable, "marbor is checking the backup directory for leftover files (after start-up or a directory change); try again in a moment")
}
