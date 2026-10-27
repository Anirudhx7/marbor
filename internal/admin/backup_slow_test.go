package admin

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// The real admin listener runs with a short ReadTimeout and WriteTimeout. These
// tests serve the backup handlers from an httptest server configured with
// scaled-down versions of those timeouts and move bytes more slowly than the
// timeouts allow, which is what a multi-gigabyte transfer over a slow link does.

const (
	slowServerTimeout = 300 * time.Millisecond
	slowChunkDelay    = 120 * time.Millisecond
)

func newSlowServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = slowServerTimeout
	srv.Config.WriteTimeout = slowServerTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func withIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := backupIdleTimeout
	backupIdleTimeout = d
	t.Cleanup(func() { backupIdleTimeout = old })
}

// slowDrain reads the whole body in 1 MiB steps, pausing between them, and
// returns the bytes it received and the elapsed time.
func slowDrain(body io.Reader) (int64, time.Duration, error) {
	start := time.Now()
	buf := make([]byte, 1<<20)
	var total int64
	for {
		n, err := io.ReadFull(body, buf)
		total += int64(n)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return total, time.Since(start), nil
		}
		if err != nil {
			return total, time.Since(start), err
		}
		time.Sleep(slowChunkDelay)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSlowUploadOutlivesServerReadTimeout(t *testing.T) {
	withIdleTimeout(t, 5*time.Second)
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(validSQLiteBytes(t))
	_ = zw.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/backup/upload", s.handleUploadBackup)
	srv := newSlowServer(t, mux)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, _ := mw.CreateFormFile("file", "slow.db.gz")
		half := gz.Len() / 2
		_, _ = part.Write(gz.Bytes()[:half])
		time.Sleep(3 * slowServerTimeout) // longer than the server's ReadTimeout
		_, _ = part.Write(gz.Bytes()[half:])
		_ = mw.Close()
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/backup/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow upload failed at the transport: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("slow upload = %d %s, want 200 (the read deadline must follow progress, not the server timeout)", resp.StatusCode, body)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if len(backupFilesIn(t, dir)) != 1 {
		t.Fatalf("slow upload stored %v, want exactly one backup (response %v)", backupFilesIn(t, dir), m)
	}
}

func TestSlowDownloadOutlivesServerWriteTimeout(t *testing.T) {
	withIdleTimeout(t, 5*time.Second)
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	name := backupFilename(time.Now())
	path := writeGz(t, dir, name, randomBytes(t, 16<<20))
	want, _ := os.Stat(path)

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/backup/download", s.handleDownloadBackup)
	srv := newSlowServer(t, mux)

	resp, err := http.Get(srv.URL + "/admin/backup/download?name=" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, elapsed, err := slowDrain(resp.Body)
	if err != nil || got != want.Size() {
		t.Fatalf("slow download got %d of %d bytes, err %v after %v; the write deadline must follow progress", got, want.Size(), err, elapsed)
	}
	if elapsed < 2*slowServerTimeout {
		t.Fatalf("download finished in %v, which does not outlast the %v server timeout; the test is not exercising the deadline", elapsed, slowServerTimeout)
	}
}

// hidingWriter is a ResponseWriter that does not expose the connection, like a
// middleware without Unwrap. It is the control for the two tests above: with the
// connection out of reach the server-wide timeout is the only limit, and the
// same slow transfer is cut.
type hidingWriter struct{ w http.ResponseWriter }

func (h hidingWriter) Header() http.Header         { return h.w.Header() }
func (h hidingWriter) Write(b []byte) (int, error) { return h.w.Write(b) }
func (h hidingWriter) WriteHeader(c int)           { h.w.WriteHeader(c) }

func TestSlowDownloadIsCutWhenTheConnectionIsHidden(t *testing.T) {
	withIdleTimeout(t, 5*time.Second)
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	name := backupFilename(time.Now())
	path := writeGz(t, dir, name, randomBytes(t, 16<<20))
	want, _ := os.Stat(path)

	srv := newSlowServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleDownloadBackup(hidingWriter{w}, r)
	}))
	resp, err := http.Get(srv.URL + "/admin/backup/download?name=" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _, drainErr := slowDrain(resp.Body)
	if drainErr == nil && got == want.Size() {
		t.Fatal("control failed: the slow download completed even though the connection was hidden, so the other tests prove nothing")
	}
}

func TestSlowBackupNowOutlivesServerWriteTimeout(t *testing.T) {
	withIdleTimeout(t, 5*time.Second)
	s, dbPath := newBackupTestServer(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE bulk (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if _, err := db.Exec(`INSERT INTO bulk VALUES (randomblob(1048576))`); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/backup", s.handleBackupNow)
	srv := newSlowServer(t, mux)

	resp, err := http.Post(srv.URL+"/admin/backup", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("backup now = %d %s", resp.StatusCode, b)
	}
	got, elapsed, err := slowDrain(resp.Body)
	if err != nil || got != resp.ContentLength {
		t.Fatalf("slow backup-now got %d of %d bytes, err %v after %v", got, resp.ContentLength, err, elapsed)
	}
	if elapsed < 2*slowServerTimeout {
		t.Fatalf("response finished in %v, which does not outlast the %v server timeout", elapsed, slowServerTimeout)
	}
}
