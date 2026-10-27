package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cliErrCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a CLIError", err)
	}
	return ce.Code, ce.Msg
}

// A 3xx answer to an upload is an error, never followed and never success.
func TestUploadBackup_RedirectIsAnError(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect target must never be contacted")
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok").UploadBackup("a.db.gz", strings.NewReader("data"), 0)
	code, msg := cliErrCode(t, err)
	if code != ExitServerError || !strings.Contains(msg, "redirect") {
		t.Fatalf("code=%d msg=%q, want a server error mentioning the redirect", code, msg)
	}
}

// A 401 on upload is an auth failure with the auth exit code.
func TestUploadBackup_UnauthorizedIsAnAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok").UploadBackup("a.db.gz", strings.NewReader("data"), 0)
	if code, _ := cliErrCode(t, err); code != ExitAuthError {
		t.Fatalf("exit code = %d, want %d", code, ExitAuthError)
	}
}

// After the whole file is sent, a server that never answers is abandoned too.
func TestUploadBackup_NoResponseAfterTheFileIsSentTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	oldIdle, oldResp := uploadIdleTimeout, uploadResponseTimeout
	uploadIdleTimeout, uploadResponseTimeout = 5*time.Second, 300*time.Millisecond
	defer func() { uploadIdleTimeout, uploadResponseTimeout = oldIdle, oldResp }()

	start := time.Now()
	_, err := NewClient(srv.URL, "tok").UploadBackup("a.db.gz", strings.NewReader("small"), 0)
	code, msg := cliErrCode(t, err)
	if code != ExitServerError || !strings.Contains(msg, "did not answer") {
		t.Fatalf("code=%d msg=%q, want a response-timeout error", code, msg)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v to give up", time.Since(start))
	}
}

// A download whose server goes quiet mid-body ends with a stall error and no
// partial file.
func TestBackupDownload_StalledBodyIsAbandonedAndCleaned(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(append(gzipMagic(), "some"...))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	old := downloadStallTimeout
	downloadStallTimeout = 300 * time.Millisecond
	defer func() { downloadStallTimeout = old }()

	dir := t.TempDir()
	_, _, err := NewClient(srv.URL, "tok").DownloadBackup("marbor-backup-20260101-000000.db.gz", filepath.Join(dir, "x.db.gz"), false)
	code, msg := cliErrCode(t, err)
	if code != ExitServerError || !strings.Contains(msg, "stalled") {
		t.Fatalf("code=%d msg=%q, want a stall error", code, msg)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files left after a stalled download: %v", entries)
	}
}

// The destination directory is validated before any request is made.
func TestBackupNow_BadOutputDirIsRefusedBeforeTheRequest(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	missing := filepath.Join(t.TempDir(), "nope", "b.db.gz")
	if _, _, err := c.BackupNow(missing, false); err == nil {
		t.Fatal("a missing output directory must be refused")
	} else if code, msg := cliErrCode(t, err); code != ExitUserError || !strings.Contains(msg, "does not exist") {
		t.Fatalf("code=%d msg=%q", code, msg)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.BackupNow(filepath.Join(file, "b.db.gz"), false); err == nil {
		t.Fatal("an output directory that is a file must be refused")
	}
	if hit {
		t.Fatal("the server was asked to make a backup although the destination was unusable")
	}
}

// The download warning goes to the injectable writer, and the directory probe
// leaves nothing behind.
func TestCheckOutputDir_LeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := checkOutputDir(filepath.Join(dir, "b.db.gz"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("probe left files: %v", entries)
	}
}

// The no-replace path never overwrites a file that exists, with the hard-link
// route and with the placeholder route alike.
func TestPlaceFile_NeverReplacesWithoutForce(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "keep.db.gz")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := placeFile(tmp, out, false, io.Discard); err == nil {
		t.Fatal("placeFile replaced an existing file")
	}
	if got, _ := os.ReadFile(out); string(got) != "old" {
		t.Errorf("existing file changed to %q", got)
	}
	free := filepath.Join(dir, "free.db.gz")
	if err := placeFile(tmp, free, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(free); string(got) != "new" {
		t.Errorf("placed file holds %q", got)
	}
}

func TestBackupProblems_ListsEverythingThatNeedsALook(t *testing.T) {
	healthy := BackupStatus{DirExists: true, Scheduled: BackupOutcome{Result: "success"}}
	if p := backupProblems([]BackupFileInfo{{Name: "a", Integrity: "ok"}}, healthy, time.Now()); len(p) != 0 {
		t.Fatalf("a healthy fleet reported problems: %v", p)
	}
	bad := BackupStatus{
		Scheduled:        BackupOutcome{Result: "failed", Error: "disk full"},
		PruneError:       "retention: x",
		UploadPruneError: "retention: y",
		LeftoverFiles:    2,
		LastRestore:      &BackupRestoreStatus{Result: "failed", Error: "boom"},
	}
	p := strings.Join(backupProblems([]BackupFileInfo{
		{Name: "dmg", Integrity: "invalid"}, {Name: "unv", Integrity: "unverified"},
	}, bad, time.Now()), "\n")
	for _, want := range []string{"disk full", "retention: x", "retention: y", "2 leftover", "boom", "dmg is damaged", "unv could not be verified"} {
		if !strings.Contains(p, want) {
			t.Errorf("problems missing %q:\n%s", want, p)
		}
	}
}

// list --check exits ExitAttention on problems and ExitOK on a clean fleet.
func TestRunBackupList_CheckExitCode(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		check      bool
		want       int
	}{
		{"clean", `{"backups":[{"name":"marbor-backup-20260101-000000.db.gz","integrity":"ok"}],"status":{"dir_exists":true,"scheduled":{"result":"success"}}}`, true, ExitOK},
		{"problem without check", `{"backups":[],"status":{"dir_exists":true,"scheduled":{"result":"failed","error":"x"}}}`, false, ExitOK},
		{"problem with check", `{"backups":[],"status":{"dir_exists":true,"scheduled":{"result":"failed","error":"x"}}}`, true, ExitAttention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/admin/v1/login" {
					http.SetCookie(w, &http.Cookie{Name: "marbor_session", Value: "tok"})
					_, _ = w.Write([]byte(`{"username":"u","role":"admin"}`))
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			flags := &globalFlags{server: srv.URL, username: "u", password: "p"}
			var out, errw bytes.Buffer
			if got := runBackupList(flags, tc.check, &out, &errw); got != tc.want {
				t.Fatalf("exit = %d, want %d\nstdout:%s\nstderr:%s", got, tc.want, out.String(), errw.String())
			}
		})
	}
}

func TestAcknowledgeRestoreResult_SendsDelete(t *testing.T) {
	var method, path, ref string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, ref = r.Method, r.URL.Path, r.URL.Query().Get("ref")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "tok").AcknowledgeRestoreResult(""); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || path != "/admin/backup/restore-result" || ref != "-" {
		t.Fatalf("sent %s %s ref=%q", method, path, ref)
	}
	if err := NewClient(srv.URL, "tok").AcknowledgeRestoreResult("0123456789abcdef"); err != nil || ref != "0123456789abcdef" {
		t.Fatalf("err=%v ref=%q", err, ref)
	}
	if err := NewClient(srv.URL, "tok").AcknowledgeRestoreResult("../x"); err == nil {
		t.Fatal("a malformed reference was sent")
	}
}
