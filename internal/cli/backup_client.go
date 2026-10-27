package cli

// backup_client.go - the Admin API client calls behind `marbor backup`: list,
// restore, the on-demand backup and stored-backup downloads (streamed to disk
// without ever overwriting a file by accident), and uploads.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// BackupFileInfo mirrors the admin API's backup list entry.
type BackupFileInfo struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	Compressed bool      `json:"compressed"`
	// Uploaded marks a file an operator uploaded (as opposed to one the
	// scheduler or an on-demand backup made).
	Uploaded bool `json:"uploaded"`
	// Integrity is "ok" (read in full and checked by the server), "unverified"
	// (looks like a backup but has not been read in full) or "invalid".
	Integrity string `json:"integrity"`
}

// BackupOutcome is what one source (scheduled or manual) last did.
type BackupOutcome struct {
	Result    string `json:"result"`
	AttemptAt string `json:"attempt_at,omitempty"`
	SuccessAt string `json:"success_at,omitempty"`
	Error     string `json:"error,omitempty"`
	// Warning marks a success that needs a look; Note says what happened after
	// the backup was made (for example a download that was cut short).
	Warning string `json:"warning,omitempty"`
	Note    string `json:"note,omitempty"`
}

// BackupRestoreStatus is the outcome of the last restore.
type BackupRestoreStatus struct {
	Result string `json:"result"`
	At     string `json:"at,omitempty"`
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// BackupStatus mirrors the "status" object of GET /admin/backup/list.
type BackupStatus struct {
	LastResult    string `json:"last_result"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastSource    string `json:"last_source,omitempty"`

	Scheduled BackupOutcome `json:"scheduled"`
	Manual    BackupOutcome `json:"manual"`

	Running        bool                 `json:"running"`
	DirExists      bool                 `json:"dir_exists"`
	DirWarning     string               `json:"dir_warning,omitempty"`
	StatusNotSaved string               `json:"status_not_saved,omitempty"`
	PruneError     string               `json:"prune_error,omitempty"`
	LeftoverFiles  int                  `json:"leftover_files,omitempty"`
	DirScanError   string               `json:"dir_scan_error,omitempty"`
	LastRestore    *BackupRestoreStatus `json:"last_restore,omitempty"`

	// RunningSince is when the running backup started. ScheduleEnabled and
	// ScheduleIntervalHours are what the scheduler is set to do.
	RunningSince          string `json:"running_since,omitempty"`
	ScheduleEnabled       bool   `json:"schedule_enabled"`
	ScheduleIntervalHours int    `json:"schedule_interval_hours,omitempty"`

	// Notice is a one-off remark (for example that the backup directory was
	// changed); UploadPruneError is the retention problem of the uploads pool.
	Notice           string `json:"notice,omitempty"`
	UploadPruneError string `json:"upload_prune_error,omitempty"`

	// Skipped counts directory entries the server listed by name but could not
	// show (not regular files, or unreadable). It is a top-level field of the
	// response, copied here so callers see it with the rest.
	Skipped int `json:"skipped,omitempty"`
}

// ListBackups calls GET /admin/backup/list.
func (c *Client) ListBackups() ([]BackupFileInfo, BackupStatus, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/backup/list", true)
	if err != nil {
		return nil, BackupStatus{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Backups []BackupFileInfo `json:"backups"`
		Status  BackupStatus     `json:"status"`
		Skipped int              `json:"skipped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, BackupStatus{}, serverErrorf("could not parse backup list response: %v", err)
	}
	out.Status.Skipped = out.Skipped
	return out.Backups, out.Status, nil
}

// RestoreBackup calls POST /admin/backup/restore - restarts marbor onto the
// named backup file (destructive; the caller is responsible for
// confirming with the operator first).
// It returns the correlation reference the server gave the restore (empty if the
// server sent none, or one that is not a plain reference).
func (c *Client) RestoreBackup(filename string) (string, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/backup/restore", map[string]string{"filename": filename})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Ref string `json:"ref"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil || !validRestoreRef(out.Ref) {
		return "", nil
	}
	return out.Ref, nil
}

// restoreRefRE is the only shape of restore reference the CLI will print or send
// back: 16 lowercase hex digits.
var restoreRefRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func validRestoreRef(ref string) bool { return restoreRefRE.MatchString(ref) }

// AcknowledgeRestoreResult calls DELETE /admin/backup/restore-result?ref= -
// clears the remembered outcome of the last restore once the operator has seen
// it. ref names the result being cleared (as shown by `backup list`); "" means a
// result that carries no reference and is sent as "-". The server refuses (409)
// when the stored result is a different one, so a result nobody has seen is never
// cleared by accident.
func (c *Client) AcknowledgeRestoreResult(ref string) error {
	if ref == "" {
		ref = "-"
	}
	if ref != "-" && !validRestoreRef(ref) {
		return userErrorf("%q is not a restore reference (16 hex digits)", ref)
	}
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/backup/restore-result?"+url.Values{"ref": {ref}}.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// DeleteBackup calls DELETE /admin/backup?name= - removes one stored backup file
// (destructive; the caller confirms with the operator first).
func (c *Client) DeleteBackup(name string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/backup?"+url.Values{"name": {name}}.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// warn returns where the CLI's non-fatal warnings go: the client's Warn writer,
// or standard error.
func (c *Client) warn() io.Writer {
	if c.Warn != nil {
		return c.Warn
	}
	return os.Stderr
}

// backupNowHeaderTimeout is how long marbor may take to start answering an
// on-demand backup. The server snapshots, compresses and verifies the whole
// database before it sends the first byte, and that work grows with the database
// (minutes for tens of millions of requests), so the limit is generous; a server
// that stays silent longer is reported instead of waited on forever.
var backupNowHeaderTimeout = 30 * time.Minute

// downloadHeaderTimeout is how long marbor may take to start sending a stored
// backup, which needs no preparation.
var downloadHeaderTimeout = 2 * time.Minute

// headerClient is the streaming client with a limit on the wait for the response
// headers (the body then has its own idle watchdog) and with transport
// decompression off, so the bytes saved are exactly the bytes the server sent.
func (c *Client) headerClient(d time.Duration) *http.Client {
	hc := c.streamingClient()
	var base *http.Transport
	switch t := hc.Transport.(type) {
	case nil:
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			base = dt.Clone()
		}
	case *http.Transport:
		base = t.Clone()
	}
	if base != nil {
		base.ResponseHeaderTimeout = d
		base.DisableCompression = true
		hc.Transport = base
	}
	return hc
}

// headerTimeoutError turns the transport's header-wait timeout into advice;
// other errors pass through.
func headerTimeoutError(err error, d time.Duration, what string) error {
	if err != nil && strings.Contains(err.Error(), "timeout awaiting response headers") {
		return serverErrorf("marbor did not start answering within %s; %s - check 'marbor backup list'", d, what)
	}
	return err
}

// downloadStallTimeout is how long a download may receive no data before it is
// abandoned and its partial file removed.
var downloadStallTimeout = 2 * time.Minute

// backupNameRE is the only shape of backup file name the CLI will use as a local
// file name when the user did not choose one: what the server's own backups are
// called, optionally with the upload marker.
var backupNameRE = regexp.MustCompile(`^marbor-backup-\d{8}-\d{6}(-upload)?\.db(\.gz)?$`)

// defaultBackupFileName is used when the server suggests no usable name.
const defaultBackupFileName = "marbor-backup.db.gz"

var (
	dispositionExtRE   = regexp.MustCompile(`filename\*\s*=\s*[^';]*'[^';]*'([^;]+)`)
	dispositionPlainRE = regexp.MustCompile(`filename\s*=\s*"?([^";]+)"?`)
)

// validBackupName reports whether name is a bare, valid backup file name.
func validBackupName(name string) bool {
	return name == filepath.Base(name) && backupNameRE.MatchString(name)
}

// suggestedBackupName returns the file name a Content-Disposition header
// suggests, but only when it is a valid backup name; anything else (a path, an
// unexpected extension) falls back to the default so a server cannot choose an
// arbitrary local file name.
func suggestedBackupName(header string) string {
	var candidates []string
	if m := dispositionExtRE.FindStringSubmatch(header); len(m) == 2 {
		if dec, err := url.PathUnescape(m[1]); err == nil {
			candidates = append(candidates, dec)
		}
	}
	if m := dispositionPlainRE.FindStringSubmatch(header); len(m) == 2 {
		candidates = append(candidates, m[1])
	}
	for _, name := range candidates {
		if validBackupName(name) {
			return name
		}
	}
	return defaultBackupFileName
}

// BackupNow calls POST /admin/backup - an on-demand backup streamed back as a
// gzip .db.gz download. The body is streamed to disk (never held in memory)
// through a temporary file that is moved into place only when the transfer
// completed, so a cut connection never leaves a plausible-looking short file.
// output empty means the server-suggested name in the current directory. An
// existing file is never replaced unless force is set. Returns the final path
// and the number of bytes written.
func (c *Client) BackupNow(output string, force bool) (string, int64, error) {
	if err := checkOutputFree(output, force); err != nil {
		return "", 0, err
	}
	// Check the destination directory before asking the server to do the work.
	if err := checkOutputDir(output, c.warn()); err != nil {
		return "", 0, err
	}
	resp, err := c.doRequestBodyWith(c.headerClient(backupNowHeaderTimeout), http.MethodPost, "/admin/backup", nil)
	if err != nil {
		return "", 0, headerTimeoutError(err, backupNowHeaderTimeout, "the backup may still have finished on the server")
	}
	defer resp.Body.Close()
	return saveBackupResponse(resp, output, force, c.warn())
}

// DownloadBackup calls GET /admin/backup/download?name= - streams a backup
// already stored on the server to a local file, same safe write as BackupNow.
func (c *Client) DownloadBackup(name, output string, force bool) (string, int64, error) {
	if err := checkOutputFree(output, force); err != nil {
		return "", 0, err
	}
	if err := checkOutputDir(output, c.warn()); err != nil {
		return "", 0, err
	}
	resp, err := c.doRequestBodyWith(c.headerClient(downloadHeaderTimeout), http.MethodGet, "/admin/backup/download?"+url.Values{"name": {name}}.Encode(), nil)
	if err != nil {
		return "", 0, headerTimeoutError(err, downloadHeaderTimeout, "nothing was saved")
	}
	defer resp.Body.Close()
	return saveBackupResponse(resp, output, force, c.warn())
}

// checkOutputFree refuses early when output names something that already
// exists and force is not set, so no backup is made or transferred for nothing.
// An empty output is checked later, once the server's name is known.
func checkOutputFree(output string, force bool) error {
	if output == "" || force {
		return nil
	}
	_, err := os.Lstat(output)
	switch {
	case err == nil:
		return userErrorf("%s already exists; choose another --output or pass --force to replace it", output)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return userErrorf("cannot check %s: %v", output, err)
	}
}

// checkOutputDir refuses early when the directory the file will be written to
// does not exist or cannot be written to, so a backup is never made or streamed
// only to fail at the last step. An empty output means the current directory.
func checkOutputDir(output string, warn io.Writer) error {
	dir := "."
	if output != "" {
		dir = filepath.Dir(output)
	}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return userErrorf("the directory %s does not exist; create it or choose another --output", dir)
	case err != nil:
		return userErrorf("cannot use the directory %s: %v", dir, err)
	case !fi.IsDir():
		return userErrorf("%s is not a directory; choose another --output", dir)
	}
	probe, err := os.CreateTemp(dir, ".marbor-backup-probe-*")
	if err != nil {
		return userErrorf("cannot write in the directory %s: %v", dir, err)
	}
	name := probe.Name()
	closeErr := probe.Close()
	if rmErr := os.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		fmt.Fprintf(warn, "warning: could not remove the probe file %s: %v\n", name, rmErr)
	}
	if closeErr != nil {
		return userErrorf("cannot write in the directory %s: %v", dir, closeErr)
	}
	return nil
}

// idleBody closes the response body when no data arrives for d, which unblocks
// the read in progress, so a download whose server went quiet ends instead of
// hanging forever.
type idleBody struct {
	rc      io.ReadCloser
	d       time.Duration
	timer   *time.Timer
	stalled atomic.Bool
}

func newIdleBody(rc io.ReadCloser, d time.Duration) *idleBody {
	b := &idleBody{rc: rc, d: d}
	b.timer = time.AfterFunc(d, func() {
		b.stalled.Store(true)
		_ = rc.Close()
	})
	b.timer.Stop()
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.d)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *idleBody) stop() { b.timer.Stop() }

// countingSink counts bytes and remembers the first error of the local write,
// so a full disk is reported as a local problem and not as a broken transfer.
type countingSink struct {
	w   io.Writer
	err error
}

func (s *countingSink) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil && s.err == nil {
		s.err = err
	}
	return n, err
}

// saveBackupResponse streams resp.Body into output (or the server-suggested
// name, if it is a valid backup name) via a temp file in the same directory.
// Without force the file is put in place without ever replacing one that
// appeared in the meantime; with force it is renamed over. Failures of the local
// disk are reported as local errors, a short, stalled or broken transfer as a
// server error. Ctrl-C (or SIGTERM) ends the transfer and removes the partial
// file. A failure to flush the directory afterwards is only a warning: the
// file is complete and in place, so the command still exits 0.
func saveBackupResponse(resp *http.Response, output string, force bool, warn io.Writer) (string, int64, error) {
	if err := checkBackupResponse(resp); err != nil {
		return "", 0, err
	}
	if output == "" {
		output = suggestedBackupName(resp.Header.Get("Content-Disposition"))
		if err := checkOutputFree(output, force); err != nil {
			return "", 0, err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".marbor-backup-download-*")
	if err != nil {
		return "", 0, userErrorf("cannot write %s: %v", output, err)
	}
	tmpPath := tmp.Name()
	finished := false
	defer func() {
		if finished {
			return
		}
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			fmt.Fprintf(warn, "warning: could not remove the temporary file %s: %v\n", tmpPath, rmErr)
		}
	}()

	var interrupted atomic.Bool
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	sigDone := make(chan struct{})
	defer func() {
		signal.Stop(sigs)
		close(sigDone)
	}()
	go func() {
		select {
		case <-sigs:
			interrupted.Store(true)
			_ = resp.Body.Close()
		case <-sigDone:
		}
	}()

	body := newIdleBody(resp.Body, downloadStallTimeout)
	defer body.stop()
	sink := &countingSink{w: tmp}
	n, copyErr := io.CopyBuffer(sink, struct{ io.Reader }{body}, make([]byte, 1<<20))
	syncErr := error(nil)
	if copyErr == nil {
		syncErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	switch {
	case interrupted.Load():
		return "", 0, userErrorf("interrupted after %d bytes; the partial file was removed and nothing was saved", n)
	case sink.err != nil:
		return "", 0, userErrorf("cannot write %s: %v", output, sink.err)
	case body.stalled.Load():
		return "", 0, serverErrorf("backup transfer stalled: no data for %s, received %d bytes; the partial file was removed", downloadStallTimeout, n)
	case copyErr != nil:
		return "", 0, serverErrorf("backup transfer failed after %d bytes: %v", n, copyErr)
	case resp.ContentLength >= 0 && resp.Header.Get("Content-Encoding") == "" && n != resp.ContentLength:
		return "", 0, serverErrorf("backup transfer incomplete: received %d of %d bytes", n, resp.ContentLength)
	case syncErr != nil:
		return "", 0, userErrorf("cannot write %s: %v", output, syncErr)
	case closeErr != nil:
		return "", 0, userErrorf("cannot write %s: %v", output, closeErr)
	}
	if err := checkBackupMagic(tmpPath); err != nil {
		return "", 0, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			return "", 0, userErrorf("cannot write %s: %v", output, err)
		}
	}
	if err := placeFile(tmpPath, output, force, warn); err != nil {
		return "", 0, err
	}
	finished = true
	syncParentDir(output, warn)
	return output, n, nil
}

// checkBackupResponse refuses a response that cannot be a backup before any of it
// is written: a status other than 200, or a type that is plainly something else
// (an error page from a proxy, a JSON body). Nothing is saved in that case.
func checkBackupResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return serverErrorf("the server answered %d instead of sending a backup; nothing was saved", resp.StatusCode)
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	switch ct {
	case "application/gzip", "application/x-gzip", "application/octet-stream", "application/vnd.sqlite3", "application/x-sqlite3":
		return nil
	}
	return serverErrorf("the server sent %q instead of a backup file (a proxy or login page in front of marbor?); nothing was saved", ct)
}

// checkBackupMagic reads the first bytes of the saved file: a backup is either
// gzip or a SQLite database, and nothing else is kept as one.
func checkBackupMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return userErrorf("cannot check the saved file: %v", err)
	}
	defer f.Close()
	head := make([]byte, 16)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return userErrorf("cannot check the saved file: %v", err)
	}
	head = head[:n]
	if (len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b) || strings.HasPrefix(string(head), "SQLite format 3\x00") {
		return nil
	}
	return serverErrorf("what the server sent is not a gzip or SQLite backup; the file was not kept")
}

// placeFile moves tmpPath to output. Without force an existing output is an
// error, never replaced - on every platform: a hard link where the filesystem
// has them, otherwise an exclusively created placeholder that is renamed over
// (the rename replaces only our own empty placeholder, so a file that appeared
// in the meantime is never overwritten, which a plain check-then-rename on
// Windows could not guarantee).
func placeFile(tmpPath, output string, force bool, warn io.Writer) error {
	if force {
		if err := os.Rename(tmpPath, output); err != nil {
			return userErrorf("cannot write %s: %v", output, err)
		}
		return nil
	}
	linkErr := os.Link(tmpPath, output)
	switch {
	case linkErr == nil:
		if err := os.Remove(tmpPath); err != nil {
			fmt.Fprintf(warn, "warning: backup saved to %s, but the temporary file %s could not be removed: %v\n", output, tmpPath, err)
		}
		return nil
	case errors.Is(linkErr, os.ErrExist):
		return userErrorf("%s already exists; choose another --output or pass --force to replace it", output)
	}
	hold, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case errors.Is(err, os.ErrExist):
		return userErrorf("%s already exists; choose another --output or pass --force to replace it", output)
	case err != nil:
		return userErrorf("cannot write %s: %v", output, err)
	}
	if err := hold.Close(); err != nil {
		_ = os.Remove(output)
		return userErrorf("cannot write %s: %v", output, err)
	}
	if err := os.Rename(tmpPath, output); err != nil {
		if rmErr := os.Remove(output); rmErr != nil {
			fmt.Fprintf(warn, "warning: could not remove the placeholder %s: %v\n", output, rmErr)
		}
		return userErrorf("cannot write %s: %v", output, err)
	}
	return nil
}

// syncParentDir flushes the directory entry of path so the saved backup survives
// a power loss. It is skipped on Windows, which cannot open a directory to sync;
// a failure is reported as a warning because the file itself is already in place.
func syncParentDir(path string, warn io.Writer) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = d.Sync()
		if cerr := d.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintf(warn, "warning: could not flush the directory of %s to disk: %v\n", path, err)
	}
}

// uploadIdleTimeout is how long an upload may go without the server accepting
// more data before it is abandoned.
var uploadIdleTimeout = 2 * time.Minute

// uploadResponseTimeout is the least time the server gets to answer once the whole
// file has been sent; uploadResponseWait grows it with the file, because the
// server reads the file back to verify it (a compressed one is unpacked first).
var uploadResponseTimeout = 15 * time.Minute

// uploadVerifyRate is the rate, in bytes per second, assumed for that
// verification when sizing the wait: the larger of the minimum above and
// size/uploadVerifyRate.
const uploadVerifyRate = 8 << 20

func uploadResponseWait(size int64) time.Duration {
	d := time.Duration(size/uploadVerifyRate) * time.Second
	if d < uploadResponseTimeout {
		return uploadResponseTimeout
	}
	return d
}

var (
	errUploadStalled    = errors.New("upload stalled")
	errUploadNoResponse = errors.New("upload response timed out")
)

// trackedReader records a failure of the local source, so it is not mistaken for
// a network failure, and tells the idle watchdog about progress.
type trackedReader struct {
	r      io.Reader
	onRead func()
	onEOF  func()

	mu  sync.Mutex
	err error
}

func (t *trackedReader) Read(p []byte) (int, error) {
	t.onRead()
	n, err := t.r.Read(p)
	switch {
	case err == io.EOF:
		t.onEOF()
	case err != nil:
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
	}
	return n, err
}

func (t *trackedReader) readErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// UploadBackup calls POST /admin/backup/upload with r's content as a
// multipart/form-data "file" field, mirroring the UI's Settings.tsx upload
// picker. Only the base name of filename is sent (informational to the server,
// which names the stored file itself). A failure reading the local file is
// reported as such, and an upload that stops making progress is abandoned after
// uploadIdleTimeout. size is the file's length (0 when unknown) and sizes the wait
// for the server's verdict. The returned warning is the server's note about a
// saved file that needs a look ("" when there is none).
func (c *Client) UploadBackup(filename string, r io.Reader, size int64) (string, error) {
	if c.Token == "" {
		return "", userErrorf("authentication required: run 'marbor login', or pass --username/--password (or set MARBOR_USERNAME+MARBOR_PASSWORD)")
	}
	base := filepath.Base(filename)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var sent atomic.Bool
	timer := time.AfterFunc(uploadIdleTimeout, func() {
		if sent.Load() {
			cancel(errUploadNoResponse)
			return
		}
		cancel(errUploadStalled)
	})
	defer timer.Stop()
	src := &trackedReader{
		r:      r,
		onRead: func() { timer.Reset(uploadIdleTimeout) },
		// The whole file is out: from now on wait for the server's verdict, which
		// may take a while for a large file, but not forever.
		onEOF: func() {
			sent.Store(true)
			timer.Reset(uploadResponseWait(size))
		},
	}

	// Stream the multipart body through a pipe so a multi-GB backup is never
	// held in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", base)
		if err == nil {
			_, err = io.Copy(part, src)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/backup/upload", pr)
	if err != nil {
		pr.Close()
		return "", userErrorf("building request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.streamingClient()
	// A redirect is never followed: the upload would be re-sent (or silently
	// turned into a GET) somewhere the operator did not choose.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		switch {
		case src.readErr() != nil:
			return "", userErrorf("cannot read %s: %v", base, src.readErr())
		case errors.Is(context.Cause(ctx), errUploadStalled):
			return "", serverErrorf("upload stalled: the server accepted no data for %s; nothing was saved", uploadIdleTimeout)
		case errors.Is(context.Cause(ctx), errUploadNoResponse):
			return "", serverErrorf("the server did not answer within %s after the whole file was sent; it may or may not have been saved - check 'marbor backup list'", uploadResponseWait(size))
		}
		return "", serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return "", serverErrorf("the server answered with a redirect (%d to %q) instead of accepting the upload; nothing was saved - check the --url and any proxy in front of marbor", resp.StatusCode, resp.Header.Get("Location"))
	case resp.StatusCode == http.StatusUnauthorized:
		return "", authErrorf("%s (run 'marbor login' again)", readErrorMessage(resp.Body))
	case resp.StatusCode >= 500:
		return "", serverErrorf("%s", readErrorMessage(resp.Body))
	case resp.StatusCode >= 400:
		return "", userErrorf("%s", readErrorMessage(resp.Body))
	}
	var out struct {
		Warning string `json:"warning"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil {
		return "", nil
	}
	return out.Warning, nil
}
