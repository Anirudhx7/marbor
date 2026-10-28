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

func TestBackupDownloadAndNow_StreamToFileAtomically(t *testing.T) {
	payload := append(gzipMagic(), bytes.Repeat([]byte("gz"), 1<<16)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/backup/download":
			if r.URL.Query().Get("name") == "" {
				http.Error(w, `{"error":"x"}`, http.StatusBadRequest)
				return
			}
		case "/admin/backup":
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="marbor-backup-20260101-000000.db.gz"`)
		w.Write(payload)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	dir := t.TempDir()
	out := filepath.Join(dir, "got.db.gz")
	path, n, err := c.DownloadBackup("marbor-backup-20260101-000000.db.gz", out, false)
	if err != nil || path != out || n != int64(len(payload)) {
		t.Fatalf("DownloadBackup = %q, %d, %v", path, n, err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, payload) {
		t.Error("downloaded bytes differ")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}

	out2 := filepath.Join(dir, "now.db.gz")
	if _, n, err := c.BackupNow(out2, false); err != nil || n != int64(len(payload)) {
		t.Fatalf("BackupNow = %d, %v", n, err)
	}
}

func TestBackupDownload_CutTransferLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "1000000")
		w.Write(append(gzipMagic(), "short"...))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	dir := t.TempDir()
	out := filepath.Join(dir, "x.db.gz")
	if _, _, err := c.DownloadBackup("marbor-backup-20260101-000000.db.gz", out, false); err == nil {
		t.Fatal("expected error for truncated transfer")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files left after failed transfer: %v", entries)
	}
}

func TestUploadBackup_StreamsMultipart(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		got, _ = io.ReadAll(f)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	c.HTTPClient.Timeout = 5 * time.Second
	body := strings.Repeat("abc", 100000)
	if _, err := c.UploadBackup("a.db.gz", strings.NewReader(body), 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Error("uploaded body differs")
	}
}

func backupServer(t *testing.T, payload []byte, disposition string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(append(gzipMagic(), payload...))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "tok")
}

func TestBackupDownload_NeverOverwritesWithoutForce(t *testing.T) {
	c := backupServer(t, []byte("new-bytes"), "")
	dir := t.TempDir()
	out := filepath.Join(dir, "keep.db.gz")
	if err := os.WriteFile(out, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.DownloadBackup("marbor-backup-20260101-000000.db.gz", out, false)
	var ce *CLIError
	if !errors.As(err, &ce) || ce.Code != ExitUserError || !strings.Contains(ce.Msg, "--force") {
		t.Fatalf("err = %v, want a user error that names --force", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "precious" {
		t.Fatalf("existing file changed to %q", got)
	}
	if _, n, err := c.DownloadBackup("marbor-backup-20260101-000000.db.gz", out, true); err != nil || n != int64(len(gzipMagic())+9) {
		t.Fatalf("forced download = %d, %v", n, err)
	}
	if got, _ := os.ReadFile(out); string(got) != string(gzipMagic())+"new-bytes" {
		t.Fatalf("forced download left %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestBackupNow_RefusesToReplaceTheServerSuggestedName(t *testing.T) {
	c := backupServer(t, []byte("x"), `attachment; filename="marbor-backup-20260101-000000.db.gz"`)
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Skip("cannot change directory")
	}
	defer os.Chdir(old)
	if err := os.WriteFile("marbor-backup-20260101-000000.db.gz", []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.BackupNow("", false); err == nil {
		t.Fatal("a backup named by the server replaced an existing file")
	}
	if got, _ := os.ReadFile("marbor-backup-20260101-000000.db.gz"); string(got) != "precious" {
		t.Fatalf("existing file changed to %q", got)
	}
}

func TestSuggestedBackupName_OnlyAcceptsValidBackupNames(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="marbor-backup-20260101-000000.db.gz"`:            "marbor-backup-20260101-000000.db.gz",
		`attachment; filename=marbor-backup-20260101-000000-upload.db`:          "marbor-backup-20260101-000000-upload.db",
		`attachment; filename*=UTF-8''marbor-backup-20260101-000000.db.gz`:      "marbor-backup-20260101-000000.db.gz",
		`attachment; filename="../../etc/cron.d/x"`:                             defaultBackupFileName,
		`attachment; filename="notes.txt"`:                                      defaultBackupFileName,
		`attachment; filename="marbor-backup-20260101-000000.db.gz.exe"`:        defaultBackupFileName,
		`attachment; filename="sub/marbor-backup-20260101-000000.db.gz"`:        defaultBackupFileName,
		`attachment; filename*=UTF-8''..%2Fmarbor-backup-20260101-000000.db.gz`: defaultBackupFileName,
		``: defaultBackupFileName,
	}
	for header, want := range cases {
		if got := suggestedBackupName(header); got != want {
			t.Errorf("suggestedBackupName(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestBackupDownload_NameArgumentMustBeABackupName(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runBackupDownload(&globalFlags{}, "../../etc/passwd", "", false, &out, &errOut)
	if code != ExitUserError || !strings.Contains(errOut.String(), "not a marbor backup file name") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
}

func TestBackupTransfer_LocalDiskErrorIsNotAServerError(t *testing.T) {
	c := backupServer(t, []byte("x"), "")
	missingDir := filepath.Join(t.TempDir(), "no", "such", "dir", "out.db.gz")
	_, _, err := c.DownloadBackup("marbor-backup-20260101-000000.db.gz", missingDir, false)
	var ce *CLIError
	if !errors.As(err, &ce) || ce.Code != ExitUserError {
		t.Fatalf("err = %v, want a local (user) error and not a server error", err)
	}
}

func TestBackupDownload_ShortBodyWithoutErrorIsStillDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, buf, _ := hj.Hijack()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nConnection: close\r\n\r\nshort")
		buf.Flush()
		conn.Close()
	}))
	defer srv.Close()
	dir := t.TempDir()
	_, _, err := NewClient(srv.URL, "tok").DownloadBackup("marbor-backup-20260101-000000.db.gz", filepath.Join(dir, "o.db.gz"), false)
	var ce *CLIError
	if !errors.As(err, &ce) || ce.Code != ExitServerError {
		t.Fatalf("err = %v, want a server error for a truncated body", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files left: %v", entries)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk read failed") }

func TestUploadBackup_LocalReadErrorIsReportedAsLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok").UploadBackup("/home/me/secret dir/a.db.gz", failingReader{}, 0)
	var ce *CLIError
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "cannot read a.db.gz") || strings.Contains(ce.Msg, "could not reach") {
		t.Fatalf("err = %v, want a local read error naming the base file name only", err)
	}
	if strings.Contains(ce.Msg, "/home/me") {
		t.Errorf("error leaks the local directory: %q", ce.Msg)
	}
}

func TestUploadBackup_SendsTheBaseFileNameOnly(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, h, err := r.FormFile("file")
		if err == nil {
			gotName = h.Filename
		}
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "tok").UploadBackup(filepath.Join("some", "dir", "mine.db.gz"), strings.NewReader("x"), 0); err != nil {
		t.Fatal(err)
	}
	if gotName != "mine.db.gz" {
		t.Errorf("sent file name %q, want the base name only", gotName)
	}
}

func TestUploadBackup_StalledServerIsAbandoned(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never reads the body
	}))
	defer srv.Close()
	defer close(release)
	old := uploadIdleTimeout
	uploadIdleTimeout = 300 * time.Millisecond
	defer func() { uploadIdleTimeout = old }()

	start := time.Now()
	_, err := NewClient(srv.URL, "tok").UploadBackup("a.db.gz", bytes.NewReader(bytes.Repeat([]byte("x"), 64<<20)), 0)
	var ce *CLIError
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "stalled") {
		t.Fatalf("err = %v, want a stalled-upload error", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v to give up", time.Since(start))
	}
}

func TestBackupList_PrintsEveryStatusConditionWithoutInvention(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"backups":[{"name":"marbor-backup-20260101-000000-upload.db.gz","size_bytes":5,"modified_at":"2026-01-01T00:00:00Z","compressed":true,"uploaded":true,"integrity":"invalid"}],` +
			`"status":{"last_result":"failed","last_source":"manual","scheduled":{"result":"success","success_at":"2026-01-01T00:00:00Z"},"manual":{"result":"failed","error":"boom"},` +
			`"dir_exists":false,"status_not_saved":"status kept in memory","prune_error":"old backups were not pruned","leftover_files":2,` +
			`"last_restore":{"result":"failed","error":"restore failed"}},"skipped":3}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	_, st, err := c.ListBackups()
	if err != nil || st.Skipped != 3 || st.LeftoverFiles != 2 || st.LastRestore == nil {
		t.Fatalf("ListBackups status = %+v, %v", st, err)
	}
	var sb strings.Builder
	printBackupStatus(&sb, st)
	for _, want := range []string{"scheduled backup: success", "manual backup: failed", "(boom)", "last restore: failed", "does not exist yet", "status kept in memory", "not pruned", "2 leftover", "3 entr"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, sb.String())
		}
	}
}

func TestListBackups_ReturnsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"backups":[{"name":"marbor-backup-20260101-000000.db.gz","size_bytes":5,"modified_at":"2026-01-01T00:00:00Z","compressed":true}],"status":{"last_result":"failed","last_error":"boom"}}`))
	}))
	defer srv.Close()
	files, st, err := NewClient(srv.URL, "tok").ListBackups()
	if err != nil || len(files) != 1 || !files[0].Compressed || st.LastResult != "failed" || st.LastError != "boom" {
		t.Fatalf("ListBackups = %+v %+v %v", files, st, err)
	}
}
