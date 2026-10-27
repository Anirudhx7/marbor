package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gzipMagic() []byte { return []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3} }

func backupResp(status int, contentType string, body []byte) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

// A response that is not a backup is refused and nothing is left behind.
func TestSaveBackupResponse_RefusesWhatIsNotABackup(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *http.Response
	}{
		{"html page", backupResp(200, "text/html", []byte("<html>login</html>"))},
		{"json body", backupResp(200, "application/json", []byte(`{"a":1}`))},
		{"no content type", backupResp(200, "", gzipMagic())},
		{"wrong status", backupResp(204, "application/gzip", nil)},
		{"right type, wrong bytes", backupResp(200, "application/gzip", []byte("this is not gzip at all"))},
		{"empty body", backupResp(200, "application/gzip", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "b.db.gz")
			if _, _, err := saveBackupResponse(tc.resp, out, false, io.Discard); err == nil {
				t.Fatal("a non-backup response was saved")
			}
			left, _ := os.ReadDir(dir)
			if len(left) != 0 {
				t.Errorf("files left behind: %v", left)
			}
		})
	}
}

func TestSaveBackupResponse_AcceptsGzipAndSQLite(t *testing.T) {
	sqlite := append([]byte("SQLite format 3\x00"), make([]byte, 100)...)
	for name, body := range map[string][]byte{"gzip": gzipMagic(), "sqlite": sqlite} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "b")
			ct := "application/gzip"
			if name == "sqlite" {
				ct = "application/octet-stream"
			}
			if _, n, err := saveBackupResponse(backupResp(200, ct+"; charset=binary", body), out, false, io.Discard); err != nil || n != int64(len(body)) {
				t.Fatalf("n=%d err=%v", n, err)
			}
		})
	}
}

// A Content-Encoding on the response means the length on the wire is not the
// length of what is written, so the length check is skipped.
func TestSaveBackupResponse_ContentEncodingSkipsTheLengthCheck(t *testing.T) {
	resp := backupResp(200, "application/gzip", gzipMagic())
	resp.ContentLength = 5
	resp.Header.Set("Content-Encoding", "identity")
	if _, _, err := saveBackupResponse(resp, filepath.Join(t.TempDir(), "b"), false, io.Discard); err != nil {
		t.Fatal(err)
	}
	resp = backupResp(200, "application/gzip", gzipMagic())
	resp.ContentLength = 500
	if _, _, err := saveBackupResponse(resp, filepath.Join(t.TempDir(), "b"), false, io.Discard); err == nil {
		t.Fatal("a short body was accepted")
	}
}

// The warnings go where the client says, not to a package-level writer.
func TestClientWarningsAreInjectable(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient("http://x", "tok")
	c.Warn = &buf
	if c.warn() != &buf {
		t.Fatal("the client's writer was not used")
	}
	c.Warn = nil
	if c.warn() != os.Stderr {
		t.Fatal("no writer must mean standard error")
	}
}

// A server that never answers the header is given up on, with advice.
func TestBackupNow_HeaderTimeout(t *testing.T) {
	old := backupNowHeaderTimeout
	backupNowHeaderTimeout = 150 * time.Millisecond
	defer func() { backupNowHeaderTimeout = old }()
	release := make(chan struct{})
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	_, _, err := NewClient(srv.URL, "tok").BackupNow(filepath.Join(t.TempDir(), "b.db.gz"), false)
	var ce *CLIError
	if !errors.As(err, &ce) || ce.Code != ExitServerError || !strings.Contains(ce.Msg, "marbor backup list") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadBackup_HeaderTimeout(t *testing.T) {
	old := downloadHeaderTimeout
	downloadHeaderTimeout = 150 * time.Millisecond
	defer func() { downloadHeaderTimeout = old }()
	release := make(chan struct{})
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	_, _, err := NewClient(srv.URL, "tok").DownloadBackup("marbor-backup-20260101-000000.db.gz", filepath.Join(t.TempDir(), "b"), false)
	if err == nil || !strings.Contains(err.Error(), "did not start answering") {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadResponseWait_GrowsWithTheFile(t *testing.T) {
	if got := uploadResponseWait(1 << 20); got != uploadResponseTimeout {
		t.Errorf("small file waits %s", got)
	}
	big := int64(200) << 30 // 200 GiB
	if got := uploadResponseWait(big); got <= uploadResponseTimeout || got != time.Duration(big/uploadVerifyRate)*time.Second {
		t.Errorf("big file waits %s", got)
	}
}

func TestUploadBackup_ReturnsTheServersWarning(t *testing.T) {
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"filename":"x","warning":"saved, but the directory could not be flushed"}`))
	}))
	defer srv.Close()
	warning, err := NewClient(srv.URL, "tok").UploadBackup("a.db.gz", strings.NewReader("data"), 4)
	if err != nil || !strings.Contains(warning, "could not be flushed") {
		t.Fatalf("warning=%q err=%v", warning, err)
	}
}

func TestDeleteBackup_SendsTheName(t *testing.T) {
	var method, name string
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, name = r.Method, r.URL.Query().Get("name")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "tok").DeleteBackup("marbor-backup-20260101-000000.db.gz"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || name != "marbor-backup-20260101-000000.db.gz" {
		t.Fatalf("sent %s name=%q", method, name)
	}
}

func TestRunBackupDelete_RefusesABadNameAndNeedsConfirmation(t *testing.T) {
	var calls atomic.Int32
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	flags := &globalFlags{server: srv.URL, username: "u", password: "p"}
	var out, errw bytes.Buffer
	if code := runBackupDelete(flags, "../etc/passwd", true, &out, &errw); code != ExitUserError {
		t.Errorf("bad name exit = %d", code)
	}
	if calls.Load() != 0 {
		t.Error("a request was sent for a bad name")
	}
}

func TestRestoreBackup_ReturnsOnlyAValidRef(t *testing.T) {
	for body, want := range map[string]string{
		`{"restarting":true,"ref":"0123456789abcdef"}`: "0123456789abcdef",
		`{"restarting":true,"ref":"not-a-ref"}`:        "",
		`{"restarting":true}`:                          "",
		`garbage`:                                      "",
	} {
		srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		got, err := NewClient(srv.URL, "tok").RestoreBackup("x")
		srv.Close()
		if err != nil || got != want {
			t.Errorf("%s: ref=%q err=%v", body, got, err)
		}
	}
}

// ---- --check rules ----

func TestBackupProblems_CheckRules(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	one := []BackupFileInfo{{Name: "a", Integrity: "ok"}}
	ok := func() BackupStatus {
		return BackupStatus{DirExists: true, ScheduleEnabled: true, ScheduleIntervalHours: 24,
			Scheduled: BackupOutcome{Result: "success", SuccessAt: at(2 * time.Hour)}}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*BackupStatus)
		backups []BackupFileInfo
		want    string
	}{
		{"no backups", func(s *BackupStatus) {}, nil, "no backups"},
		{"dir missing", func(s *BackupStatus) { s.DirExists = false }, one, "does not exist"},
		{"scheduled never succeeded", func(s *BackupStatus) { s.Scheduled = BackupOutcome{} }, one, "none has succeeded"},
		{"scheduled overdue", func(s *BackupStatus) { s.Scheduled.SuccessAt = at(30 * time.Hour) }, one, "overdue"},
		{"manual note", func(s *BackupStatus) { s.Manual = BackupOutcome{Result: "success", Note: "the download was cut short"} }, one, "cut short"},
		{"notice", func(s *BackupStatus) { s.Notice = "the backup directory was changed" }, one, "directory was changed"},
		{"skipped entries", func(s *BackupStatus) { s.Skipped = 3 }, one, "3 entr"},
		{"scan error", func(s *BackupStatus) { s.DirScanError = "the backup directory could not be read" }, one, "could not be read"},
		{"restore note", func(s *BackupStatus) {
			s.LastRestore = &BackupRestoreStatus{Result: "success", Note: "kept the old file"}
		}, one, "with a note"},
		{"cancelled", func(s *BackupStatus) { s.Manual = BackupOutcome{Result: "cancelled"} }, one, "cancelled"},
		{"running long", func(s *BackupStatus) { s.Running = true; s.RunningSince = at(3 * time.Hour) }, one, "running for"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ok()
			tc.mutate(&s)
			p := strings.Join(backupProblems(tc.backups, s, now), "\n")
			if !strings.Contains(p, tc.want) {
				t.Fatalf("want %q in:\n%s", tc.want, p)
			}
		})
	}
	// Healthy, a schedule that is off, and a fresh running backup raise nothing.
	if p := backupProblems(one, ok(), now); len(p) != 0 {
		t.Errorf("healthy: %v", p)
	}
	off := ok()
	off.ScheduleEnabled = false
	off.Scheduled = BackupOutcome{}
	if p := backupProblems(one, off, now); len(p) != 0 {
		t.Errorf("schedule off: %v", p)
	}
	run := ok()
	run.Running, run.RunningSince = true, at(5*time.Minute)
	if p := backupProblems(one, run, now); len(p) != 0 {
		t.Errorf("fresh running: %v", p)
	}
}

// The reasons reach stderr in JSON mode too, and the exit code is 3.
func TestRunBackupList_CheckJSONPrintsReasonsOnStderr(t *testing.T) {
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"backups":[],"status":{"dir_exists":true}}`))
	}))
	defer srv.Close()
	flags := &globalFlags{server: srv.URL, username: "u", password: "p", jsonOutput: true}
	var out, errw bytes.Buffer
	if code := runBackupList(flags, true, &out, &errw); code != ExitAttention {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errw.String(), "no backups") {
		t.Errorf("stderr = %q", errw.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Errorf("stdout is not JSON: %v", err)
	}
}

func TestPrintBackupStatus_ShowsOnlyAValidRef(t *testing.T) {
	var buf bytes.Buffer
	printBackupStatus(&buf, BackupStatus{DirExists: true, LastRestore: &BackupRestoreStatus{Result: "success", Ref: "0123456789abcdef"}})
	if !strings.Contains(buf.String(), "[ref 0123456789abcdef]") {
		t.Errorf("valid ref not shown: %s", buf.String())
	}
	buf.Reset()
	printBackupStatus(&buf, BackupStatus{DirExists: true, LastRestore: &BackupRestoreStatus{Result: "success", Ref: "evil\x1b[31m"}})
	if strings.Contains(buf.String(), "evil") || !strings.Contains(buf.String(), "ref not shown") {
		t.Errorf("an invalid ref was printed: %q", buf.String())
	}
}

// ---- restore --wait ----

type restoreSrv struct {
	startedAt atomic.Value // string
	restore   atomic.Value // *BackupRestoreStatus or nil
	authFails atomic.Bool
	down      atomic.Bool
}

func (rs *restoreSrv) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			if rs.down.Load() {
				hj, _ := w.(http.Hijacker)
				c, _, _ := hj.Hijack()
				_ = c.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "started_at": rs.startedAt.Load()})
		case r.URL.Path == "/admin/backup/restore":
			_, _ = w.Write([]byte(`{"restarting":true,"ref":"0123456789abcdef"}`))
		case r.URL.Path == "/admin/backup/list":
			if rs.authFails.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"session expired"}`))
				return
			}
			st := map[string]any{"dir_exists": true}
			if v, _ := rs.restore.Load().(*BackupRestoreStatus); v != nil {
				st["last_restore"] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"backups": []any{}, "status": st})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func fastRestoreWait(t *testing.T) {
	t.Helper()
	oi, ot := restorePollInterval, restoreWaitTimeout
	restorePollInterval, restoreWaitTimeout = 10*time.Millisecond, 2*time.Second
	t.Cleanup(func() { restorePollInterval, restoreWaitTimeout = oi, ot })
}

func runWait(t *testing.T, rs *restoreSrv, onRestore func()) (int, string, string) {
	t.Helper()
	fastRestoreWait(t)
	srv := newBackupTestServer(rs.handler())
	defer srv.Close()
	flags := &globalFlags{server: srv.URL, username: "u", password: "p"}
	done := make(chan int, 1)
	var out, errw bytes.Buffer
	go func() {
		done <- runBackupRestore(flags, "marbor-backup-20260101-000000.db.gz", true, true, &out, &errw)
	}()
	go onRestore()
	select {
	case code := <-done:
		return code, out.String(), errw.String()
	case <-time.After(10 * time.Second):
		t.Fatal("restore --wait never returned")
		return 0, "", ""
	}
}

func TestRestoreWait_SuccessIsReadFromMarbor(t *testing.T) {
	rs := &restoreSrv{}
	rs.startedAt.Store("run-1")
	code, out, errw := runWait(t, rs, func() {
		time.Sleep(60 * time.Millisecond)
		rs.down.Store(true)
		time.Sleep(60 * time.Millisecond)
		rs.restore.Store(&BackupRestoreStatus{Result: "success", Ref: "0123456789abcdef"})
		rs.startedAt.Store("run-2")
		rs.down.Store(false)
	})
	if code != ExitOK || !strings.Contains(out, "restore finished") || !strings.Contains(out, "0123456789abcdef") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errw)
	}
}

// A 503 is a marbor that is up but degraded, and an unchanged start time is the
// old process: neither counts as "back".
func TestRestoreWait_OldProcessIsNotTheRestart(t *testing.T) {
	rs := &restoreSrv{}
	rs.startedAt.Store("run-1")
	rs.restore.Store(&BackupRestoreStatus{Result: "success", Ref: "0123456789abcdef"})
	fastRestoreWait(t)
	restoreWaitTimeout = 300 * time.Millisecond
	srv := newBackupTestServer(rs.handler())
	defer srv.Close()
	flags := &globalFlags{server: srv.URL, username: "u", password: "p"}
	var out, errw bytes.Buffer
	code := runBackupRestore(flags, "marbor-backup-20260101-000000.db.gz", true, true, &out, &errw)
	if code != ExitServerError || !strings.Contains(errw.String(), "did not come back") {
		t.Fatalf("code=%d err=%q", code, errw.String())
	}
}

func TestRestoreWait_FailureExitsNonzero(t *testing.T) {
	rs := &restoreSrv{}
	rs.startedAt.Store("run-1")
	code, _, errw := runWait(t, rs, func() {
		time.Sleep(60 * time.Millisecond)
		rs.restore.Store(&BackupRestoreStatus{Result: "failed", Error: "the file was damaged", Ref: "0123456789abcdef"})
		rs.startedAt.Store("run-2")
	})
	if code != ExitServerError || !strings.Contains(errw, "the file was damaged") {
		t.Fatalf("code=%d err=%q", code, errw)
	}
}

func TestRestoreWait_ResultOfAnotherRestoreIsNotTrusted(t *testing.T) {
	rs := &restoreSrv{}
	rs.startedAt.Store("run-1")
	code, _, errw := runWait(t, rs, func() {
		time.Sleep(40 * time.Millisecond)
		rs.restore.Store(&BackupRestoreStatus{Result: "success", Ref: "ffffffffffffffff"})
		rs.startedAt.Store("run-2")
	})
	if code != ExitServerError || !strings.Contains(errw, "did not report the result of this restore") {
		t.Fatalf("code=%d err=%q", code, errw)
	}
}

// A session that is gone after the restart means the restored database is in
// use: reported as success, but marked unconfirmed.
func TestRestoreWait_LostSessionIsAnUnconfirmedSuccess(t *testing.T) {
	rs := &restoreSrv{}
	rs.startedAt.Store("run-1")
	code, out, _ := runWait(t, rs, func() {
		time.Sleep(40 * time.Millisecond)
		rs.authFails.Store(true)
		rs.startedAt.Store("run-2")
	})
	if code != ExitOK || !strings.Contains(out, "marbor login") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestRunBackupRestore_WithoutWaitPrintsTheRef(t *testing.T) {
	rs := &restoreSrv{}
	rs.startedAt.Store("run-1")
	srv := newBackupTestServer(rs.handler())
	defer srv.Close()
	flags := &globalFlags{server: srv.URL, username: "u", password: "p"}
	var out, errw bytes.Buffer
	if code := runBackupRestore(flags, "marbor-backup-20260101-000000.db.gz", true, false, &out, &errw); code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "ref 0123456789abcdef") {
		t.Errorf("out = %q", out.String())
	}
}

// ack-restore shows what it clears and sends the reference of that result.
func TestRunBackupAck_SendsTheDisplayedRef(t *testing.T) {
	var gotRef string
	srv := newBackupTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"backups":[],"status":{"dir_exists":true,"last_restore":{"result":"failed","error":"x","ref":"0123456789abcdef"}}}`))
		case http.MethodDelete:
			gotRef = r.URL.Query().Get("ref")
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	flags := &globalFlags{server: srv.URL, username: "u", password: "p"}
	var out, errw bytes.Buffer
	if code := runBackupAck(flags, &out, &errw); code != ExitOK || gotRef != "0123456789abcdef" {
		t.Fatalf("code=%d ref=%q err=%q", code, gotRef, errw.String())
	}
	if !strings.Contains(out.String(), "failed") {
		t.Errorf("the cleared result was not shown: %q", out.String())
	}
}

// newBackupTestServer is an httptest server that also answers the CLI's login, so
// a command run with --username/--password reaches the handler under test.
func newBackupTestServer(h http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/login" {
			http.SetCookie(w, &http.Cookie{Name: "marbor_session", Value: "tok"})
			_, _ = w.Write([]byte(`{"username":"u","role":"admin"}`))
			return
		}
		h.ServeHTTP(w, r)
	}))
}
