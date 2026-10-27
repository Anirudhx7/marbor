package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// legacyBackupFilename is the pre-compression name shape; kept so tests can
// create old plain backups next to new compressed ones.
func legacyBackupFilename(t time.Time) string {
	return strings.TrimSuffix(backupFilename(t), ".gz")
}

// newBackupTestServer is newRealStoreTestServer with the database path
// exposed, so tests can seed audit-log-shaped rows through a second handle.
func newBackupTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "live.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{}, st), dbPath
}

func seedRequestLog(t *testing.T, dbPath string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO request_log (id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts, routing_reason, routing_detail) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("req-%08d", i), "key-a", "llama3:8b", "node-1", 200, 120+i%50, 300+i%200, 0.0001*float64(i%9), "node-1", 0, time.Now().Unix()-int64(i), "warm", fmt.Sprintf("warm resident on node-1, queue depth %d, score %d", i%7, 40+i%50)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func tableCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, n := range names {
		var c int
		if err := db.QueryRow(`SELECT count(*) FROM "` + n + `"`).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", n, err)
		}
		out[n] = c
	}
	return out
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func backupFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range dirNames(t, dir) {
		if backupFilenameRE.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

func withFreeBytes(t *testing.T, free uint64) {
	t.Helper()
	old := backupFreeBytes
	backupFreeBytes = func(string) (uint64, error) { return free, nil }
	t.Cleanup(func() { backupFreeBytes = old })
}

func withDecompressCap(t *testing.T, n int64) {
	t.Helper()
	old := maxDecompressedBackupBytes
	maxDecompressedBackupBytes = n
	t.Cleanup(func() { maxDecompressedBackupBytes = old })
}

// writeGz writes payload as a marbor-style compressed backup named name in dir.
func writeGz(t *testing.T, dir, name string, payload []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CompressBackup(bytes.NewReader(payload), f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func TestBackupRoundTrip_GzipKeepsEveryTableRowCount(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	if err := s.st.UpsertNode(store.NodeRecord{Name: "node-a", URL: "http://localhost:11434", Runtime: "ollama"}); err != nil {
		t.Fatal(err)
	}
	seedRequestLog(t, dbPath, 4000)
	want := tableCounts(t, dbPath)
	dir := t.TempDir()

	if err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 7}); err != nil {
		t.Fatalf("runScheduledBackup: %v", err)
	}
	files := backupFilesIn(t, dir)
	if len(files) != 1 || !strings.HasSuffix(files[0], ".db.gz") {
		t.Fatalf("backup files = %v, want exactly one .db.gz", files)
	}
	gz := filepath.Join(dir, files[0])
	restored := filepath.Join(t.TempDir(), "restored.db")
	if _, _, err := store.DecompressBackup(context.Background(), gz, restored, 1<<30); err != nil {
		t.Fatalf("DecompressBackup: %v", err)
	}
	if err := store.ValidateBackupFile(restored); err != nil {
		t.Fatalf("restored backup invalid: %v", err)
	}
	got := tableCounts(t, restored)
	if len(got) != len(want) {
		t.Fatalf("table sets differ: got %d tables, want %d", len(got), len(want))
	}
	for table, n := range want {
		if got[table] != n {
			t.Errorf("table %s: restored %d rows, live %d", table, got[table], n)
		}
	}
	gzInfo, _ := os.Stat(gz)
	rawInfo, _ := os.Stat(restored)
	if gzInfo.Size() >= rawInfo.Size() {
		t.Errorf("compressed %d bytes not smaller than raw %d", gzInfo.Size(), rawInfo.Size())
	}
	// Only the backup and the status file may remain: no scratch, no tmp.
	for _, n := range dirNames(t, dir) {
		if n != files[0] && n != backupStatusFileName {
			t.Errorf("unexpected leftover %q after a successful run", n)
		}
	}
}

func TestHandleBackupNow_StreamsGzipOfValidDatabase(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	seedRequestLog(t, dbPath, 500)
	rec := httptest.NewRecorder()
	s.handleBackupNow(rec, httptest.NewRequest(http.MethodPost, "/admin/backup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".db.gz") {
		t.Errorf("Content-Disposition = %q, want a .db.gz filename", cd)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte("SQLite format 3")) {
		t.Error("decompressed body is not a SQLite database")
	}
}

func TestScheduledBackup_DedupUsesFingerprintNotACopy(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	seedRequestLog(t, dbPath, 300)
	dir := t.TempDir()
	cfg := config.BackupConfig{TargetDir: dir, RetentionCount: 7}

	if err := s.runScheduledBackup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.runScheduledBackup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(backupFilesIn(t, dir)); n != 1 {
		t.Fatalf("unchanged DB produced %d backups, want 1 (dedup)", n)
	}
	seedRequestLog(t, dbPath, 0)
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec(`UPDATE request_log SET latency_ms = latency_ms + 1 WHERE id = 'req-00000001'`)
	db.Close()
	if err := s.runScheduledBackup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(backupFilesIn(t, dir)); n != 2 {
		t.Fatalf("changed DB produced %d backups total, want 2", n)
	}
}

func TestPruneOldBackups_MixedPlainAndGzip(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	valid := validSQLiteBytes(t)
	var all []string
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		name := backupFilename(ts)
		all = append(all, name)
		if i%2 == 0 {
			name = legacyBackupFilename(ts)
			all[len(all)-1] = name
			if err := os.WriteFile(filepath.Join(dir, name), valid, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writeGz(t, dir, name, valid)
	}
	s.pruneBackups(context.Background(), dir, 4, false)
	got := backupFilesIn(t, dir)
	sort.Strings(all)
	want := all[2:]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after prune = %v, want newest four %v", got, want)
	}
}

func TestListBackups_ShowsBothTypesWithStatus(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	_ = os.WriteFile(filepath.Join(dir, legacyBackupFilename(ts)), []byte("plain"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, backupFilename(ts.Add(time.Hour))), []byte("gz"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "marbor-backup-20260730-130000.db.gz.tmp"), []byte("t"), 0o600)

	rec := httptest.NewRecorder()
	s.handleListBackups(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/list", nil))
	var resp struct {
		Backups []struct {
			Name       string `json:"name"`
			Compressed bool   `json:"compressed"`
		} `json:"backups"`
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Backups) != 2 || !resp.Backups[0].Compressed || resp.Backups[1].Compressed {
		t.Fatalf("backups = %+v, want newest compressed first then the plain one", resp.Backups)
	}
	if resp.Status == nil {
		t.Error("list response must carry a status object")
	}
}

func TestDownloadBackup_ValidatesNameAgainstListedSet(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	gzName, plainName := backupFilename(ts), legacyBackupFilename(ts.Add(time.Hour))
	_ = os.WriteFile(filepath.Join(dir, gzName), []byte("gzbytes"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, plainName), []byte("plainbytes"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o600)

	get := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleDownloadBackup(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/download?name="+url.QueryEscape(name), nil))
		return rec
	}
	if rec := get(gzName); rec.Code != 200 || rec.Body.String() != "gzbytes" || !strings.Contains(rec.Header().Get("Content-Disposition"), gzName) {
		t.Errorf("gz download = %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Disposition"))
	}
	if rec := get(plainName); rec.Code != 200 || rec.Body.String() != "plainbytes" {
		t.Errorf("plain download = %d %q", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"../etc/passwd", "..\\x", "/etc/passwd", "sub/" + gzName, "secret.txt", "", gzName + "/", "marbor-backup-20260730-130000.db.gz.tmp"} {
		if rec := get(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: status %d, want 400", bad, rec.Code)
		}
	}
	if rec := get(backupFilename(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))); rec.Code != http.StatusNotFound {
		t.Errorf("unknown valid-shaped name: status %d, want 404", rec.Code)
	}
}

func TestDownloadBackup_RejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	outside := filepath.Join(t.TempDir(), "outside")
	_ = os.WriteFile(outside, []byte("private"), 0o600)
	name := backupFilename(time.Now())
	if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleDownloadBackup(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/download?name="+name, nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "private") {
		t.Fatalf("symlink served: %d %q", rec.Code, rec.Body.String())
	}
}

func restoreRequest(s *Server, name string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"filename": name})
	rec := httptest.NewRecorder()
	s.handleRestoreBackup(rec, httptest.NewRequest(http.MethodPost, "/admin/backup/restore", bytes.NewReader(body)))
	return rec
}

func TestRestore_AcceptsGzipAndPlain_RejectsBadGzipBeforeHandoff(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ch := make(chan string, 4)
	s.SetRestoreChannel(ch)
	valid := validSQLiteBytes(t)
	ts := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	gzName := backupFilename(ts)
	writeGz(t, dir, gzName, valid)
	plainName := legacyBackupFilename(ts.Add(time.Hour))
	_ = os.WriteFile(filepath.Join(dir, plainName), valid, 0o600)

	truncName := backupFilename(ts.Add(2 * time.Hour))
	full, _ := os.ReadFile(filepath.Join(dir, gzName))
	_ = os.WriteFile(filepath.Join(dir, truncName), full[:len(full)/2], 0o600)

	fakeName := backupFilename(ts.Add(3 * time.Hour))
	_ = os.WriteFile(filepath.Join(dir, fakeName), valid, 0o600) // plain bytes named .db.gz

	if rec := restoreRequest(s, truncName); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("truncated gzip: %d %s, want 422", rec.Code, rec.Body.String())
	}
	if rec := restoreRequest(s, fakeName); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("non-gzip named .db.gz: %d %s, want 422", rec.Code, rec.Body.String())
	}
	if len(ch) != 0 {
		t.Fatal("a rejected restore must never reach the restart channel")
	}
	for _, n := range dirNames(t, dir) {
		if strings.Contains(n, "scratch") || strings.HasSuffix(n, ".tmp") {
			t.Errorf("validation left %q behind", n)
		}
	}
	if rec := restoreRequest(s, gzName); rec.Code != http.StatusAccepted {
		t.Errorf("valid gzip: %d %s, want 202", rec.Code, rec.Body.String())
	}
	staged := <-ch
	if !strings.HasPrefix(filepath.Base(staged), "live.db.restoring-") {
		t.Errorf("handoff path = %q, want a uniquely named staged copy next to the live database", staged)
	}
	if err := store.ValidateBackupFile(staged); err != nil {
		t.Errorf("staged restore file is not a valid database: %v", err)
	}
	// Only one restore can be in flight: a second request must not touch the
	// staged file of the first, and never reaches the restart channel.
	if rec := restoreRequest(s, plainName); rec.Code != http.StatusConflict {
		t.Errorf("second restore while one is pending: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if len(ch) != 0 {
		t.Fatal("a refused second restore reached the restart channel")
	}
	if err := store.ValidateBackupFile(staged); err != nil {
		t.Errorf("the first staged file was disturbed by the refused second restore: %v", err)
	}
}

func TestRestore_DecompressedSizeCap(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	ch := make(chan string, 1)
	s.SetRestoreChannel(ch)
	name := backupFilename(time.Now())
	writeGz(t, dir, name, validSQLiteBytes(t))
	withDecompressCap(t, 512)
	rec := restoreRequest(s, name)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(strings.ToLower(rec.Body.String()), "large") {
		t.Fatalf("cap exceeded: %d %s, want 422 mentioning size", rec.Code, rec.Body.String())
	}
	if len(ch) != 0 {
		t.Fatal("over-cap restore reached the channel")
	}
}

func TestUpload_GzipAndPlain_FingerprintDedupAndRejections(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	valid := validSQLiteBytes(t)

	var gzBuf bytes.Buffer
	if _, _, err := store.CompressBackup(bytes.NewReader(valid), &gzBuf); err != nil {
		t.Fatal(err)
	}
	upload := func(name string, content []byte) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		s.handleUploadBackup(rec, newUploadRequest(t, name, content))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec, m
	}

	rec, m := upload("x.db.gz", gzBuf.Bytes())
	if rec.Code != 200 || !strings.HasSuffix(fmt.Sprint(m["filename"]), ".db.gz") {
		t.Fatalf("gz upload: %d %v", rec.Code, m)
	}
	first := fmt.Sprint(m["filename"])
	// Same database uploaded again as a plain file is the same content.
	rec, m = upload("x.db", valid)
	if rec.Code != 200 || m["duplicate"] != true || fmt.Sprint(m["filename"]) != first {
		t.Fatalf("plain re-upload of same content: %d %v, want duplicate of %s", rec.Code, m, first)
	}
	if n := len(backupFilesIn(t, dir)); n != 1 {
		t.Fatalf("%d backups after duplicate upload, want 1", n)
	}

	data := gzBuf.Bytes()
	if rec, _ := upload("t.db.gz", data[:len(data)/2]); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("truncated gzip upload: %d, want 422", rec.Code)
	}
	if rec, _ := upload("n.db.gz", []byte("plain text pretending to be gzip")); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("wrong magic upload: %d, want 422", rec.Code)
	}
	withDecompressCap(t, 512)
	if rec, _ := upload("big.db.gz", gzBuf.Bytes()); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("over-cap gzip upload: %d, want 422", rec.Code)
	}
	for _, n := range dirNames(t, dir) {
		if strings.HasSuffix(n, ".tmp") || strings.Contains(n, "scratch") {
			t.Errorf("rejected upload left %q", n)
		}
	}
}

func TestBackupDirAndFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on windows")
	}
	s, _ := newBackupTestServer(t)
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 7}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("new backup dir mode = %o, want 0700", fi.Mode().Perm())
	}
	for _, n := range dirNames(t, dir) {
		fi, _ := os.Stat(filepath.Join(dir, n))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 0600", n, fi.Mode().Perm())
		}
	}

	broad := t.TempDir()
	if err := os.Chmod(broad, 0o755); err != nil {
		t.Fatal(err)
	}
	setBackupTargetDir(s, broad)
	if err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: broad, RetentionCount: 7}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(broad); fi.Mode().Perm() != 0o755 {
		t.Errorf("existing dir mode changed to %o; it must only be reported", fi.Mode().Perm())
	}
	rec := httptest.NewRecorder()
	s.handleListBackups(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/list", nil))
	var resp struct {
		Status struct {
			DirWarning string `json:"dir_warning"`
		} `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Status.DirWarning, "755") {
		t.Errorf("dir_warning = %q, want it to name the broad mode", resp.Status.DirWarning)
	}
}

func TestBackup_RefusedWhenDiskTooSmall_WritesNothing(t *testing.T) {
	s, dbPath := newBackupTestServer(t)
	seedRequestLog(t, dbPath, 200)
	dir := t.TempDir()
	setBackupTargetDir(s, dir)
	withFreeBytes(t, 1<<20)

	err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 7})
	if err == nil || !errors.Is(err, errBackupRefused) {
		t.Fatalf("err = %v, want errBackupRefused", err)
	}
	for _, n := range dirNames(t, dir) {
		if n != backupStatusFileName {
			t.Errorf("refused run wrote %q", n)
		}
	}
	rec := httptest.NewRecorder()
	s.handleListBackups(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/list", nil))
	var resp struct {
		Status struct {
			LastResult string `json:"last_result"`
			LastError  string `json:"last_error"`
		} `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status.LastResult != "refused" || !strings.Contains(strings.ToLower(resp.Status.LastError), "disk") {
		t.Errorf("status = %+v, want refused with a disk reason", resp.Status)
	}

	rec = httptest.NewRecorder()
	s.handleBackupNow(rec, httptest.NewRequest(http.MethodPost, "/admin/backup", nil))
	if rec.Code != http.StatusInsufficientStorage {
		t.Errorf("manual backup under disk pressure: %d, want 507", rec.Code)
	}
}

type failingBackupStore struct{ store.Store }

func (failingBackupStore) BackupTo(_ context.Context, path string) error {
	_ = os.WriteFile(path, []byte("half-written snapshot"), 0o600)
	return errors.New("simulated VACUUM INTO failure")
}

func TestBackup_FailureIsRecordedVisibleAndCleanedUp(t *testing.T) {
	base, _ := newBackupTestServer(t)
	s := NewServer(base.router, nil, config.Config{}, failingBackupStore{base.st})
	dir := t.TempDir()
	setBackupTargetDir(s, dir)

	if err := s.runScheduledBackup(context.Background(), config.BackupConfig{TargetDir: dir, RetentionCount: 7}); err == nil {
		t.Fatal("expected failure")
	}
	for _, n := range dirNames(t, dir) {
		if n != backupStatusFileName {
			t.Errorf("failed run left %q (silent .tmp leftovers are the bug)", n)
		}
	}
	rec := httptest.NewRecorder()
	s.handleListBackups(rec, httptest.NewRequest(http.MethodGet, "/admin/backup/list", nil))
	var resp struct {
		Status struct {
			LastResult string `json:"last_result"`
			LastError  string `json:"last_error"`
		} `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status.LastResult != "failed" || !strings.Contains(resp.Status.LastError, "snapshot") {
		t.Errorf("status = %+v, want failed with a fixed reason", resp.Status)
	}
	if strings.Contains(resp.Status.LastError, "simulated") || strings.Contains(resp.Status.LastError, dir) {
		t.Errorf("status error %q leaks internal detail", resp.Status.LastError)
	}
	// A restart (new Server, same dir) still reports the failure.
	s2 := NewServer(base.router, nil, config.Config{}, base.st)
	if got := s2.backupStatusSnapshot(dir); got.LastResult != "failed" {
		t.Errorf("status after restart = %q, want failed", got.LastResult)
	}
}

func TestCleanupBackupLeftovers(t *testing.T) {
	dir := t.TempDir()
	dead := []string{
		"marbor-backup-20260101-000000.db.gz.tmp",
		"marbor-backup-20260101-000000.db.tmp",
		"marbor-backup-scratch-123456.db",
		"marbor-backup-scratch-123456.db-journal",
		"marbor-backup-upload-555.tmp",
		"marbor-backup-manual-777.tmp",
		"marbor-backup-write-888.tmp",
		"marbor-backup-marker-999.tmp",
	}
	// Names that only look like ours are never touched.
	keep := []string{backupFilename(time.Now()), "notes.txt", backupStatusFileName,
		"marbor-backup-notes.tmp", "marbor-backup-scratch-abc.db", "marbor-backup-write-x.tmp", "marbor-backup-upload-.tmp"}
	for _, n := range append(append([]string{}, dead...), keep...) {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	if stuck, _ := cleanupBackupLeftovers(dir); stuck != 0 {
		t.Fatalf("%d leftovers could not be removed", stuck)
	}
	got := dirNames(t, dir)
	sort.Strings(keep)
	if strings.Join(got, ",") != strings.Join(keep, ",") {
		t.Fatalf("after cleanup = %v, want %v", got, keep)
	}
}

func TestManualBackupRefusedWhileAnotherRuns(t *testing.T) {
	s, _ := newBackupTestServer(t)
	if !s.backupRunMu.TryLock() {
		t.Fatal("setup: lock should be free")
	}
	defer s.backupRunMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleBackupNow(rec, httptest.NewRequest(http.MethodPost, "/admin/backup", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestScheduledBackupDue(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		success  time.Time
		attempt  time.Time
		result   string
		interval int
		want     bool
	}{
		{"never ran", time.Time{}, time.Time{}, "", 24, true},
		{"fresh success", now.Add(-time.Hour), now.Add(-time.Hour), "success", 24, false},
		{"old success", now.Add(-25 * time.Hour), now.Add(-25 * time.Hour), "success", 24, true},
		{"failed 10 min ago backs off", now.Add(-48 * time.Hour), now.Add(-10 * time.Minute), "failed", 24, false},
		{"refused 10 min ago backs off", time.Time{}, now.Add(-10 * time.Minute), "refused", 24, false},
		{"failed 2h ago retries", now.Add(-48 * time.Hour), now.Add(-2 * time.Hour), "failed", 24, true},
	}
	for _, c := range cases {
		if got := scheduledBackupDue(now, c.success, c.attempt, c.result, c.interval); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}
