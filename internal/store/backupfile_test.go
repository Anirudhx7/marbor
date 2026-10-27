package store_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

// gzBackup compresses payload the way a scheduled backup does and returns the
// file path.
func gzBackup(t *testing.T, payload []byte) (path, fp string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "b.db.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fp, n, err := store.CompressBackup(bytes.NewReader(payload), f)
	if err != nil {
		t.Fatalf("CompressBackup: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("raw bytes = %d, want %d", n, len(payload))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path, fp
}

func sqliteLike(size int) []byte {
	b := bytes.Repeat([]byte("row-data-0123456789 "), size/20+1)[:size]
	copy(b, "SQLite format 3\x00")
	return b
}

func TestCompressDecompressRoundTrip(t *testing.T) {
	payload := sqliteLike(300_000)
	gz, fp := gzBackup(t, payload)

	out := filepath.Join(t.TempDir(), "out.db")
	gotFP, n, err := store.DecompressBackup(context.Background(), gz, out, 1<<20)
	if err != nil {
		t.Fatalf("DecompressBackup: %v", err)
	}
	if n != int64(len(payload)) || gotFP != fp {
		t.Errorf("n=%d fp=%s, want n=%d fp=%s", n, gotFP, len(payload), fp)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, payload) {
		t.Error("round-tripped bytes differ from the original")
	}
	st, _ := os.Stat(gz)
	if st.Size() >= int64(len(payload)) {
		t.Errorf("compressed size %d not smaller than raw %d", st.Size(), len(payload))
	}
}

func TestFingerprintTailIsReadWithoutDecompressing(t *testing.T) {
	gz, fp := gzBackup(t, sqliteLike(5000))
	got, ok := store.ReadFingerprintTail(gz)
	if !ok || got != fp {
		t.Fatalf("ReadFingerprintTail = %q, %v; want %q, true", got, ok, fp)
	}
	plain := filepath.Join(t.TempDir(), "p.db")
	_ = os.WriteFile(plain, sqliteLike(5000), 0o600)
	if _, ok := store.ReadFingerprintTail(plain); ok {
		t.Error("a plain file must not report a fingerprint tail")
	}
}

func TestFingerprintIgnoresHeaderChangeCounter(t *testing.T) {
	a := sqliteLike(4096)
	b := append([]byte(nil), a...)
	b[24], b[27], b[92], b[95] = 7, 9, 3, 5 // file change counter + version-valid-for
	_, fpA := gzBackup(t, a)
	_, fpB := gzBackup(t, b)
	if fpA != fpB {
		t.Error("fingerprint must ignore the header change counter bytes")
	}
	c := append([]byte(nil), a...)
	c[2000] ^= 0xff
	_, fpC := gzBackup(t, c)
	if fpA == fpC {
		t.Error("fingerprint must change when real content changes")
	}
}

func TestDecompressRejectsTruncatedGzip(t *testing.T) {
	gz, _ := gzBackup(t, sqliteLike(200_000))
	data, _ := os.ReadFile(gz)
	trunc := filepath.Join(t.TempDir(), "t.db.gz")
	_ = os.WriteFile(trunc, data[:len(data)/2], 0o600)
	out := filepath.Join(t.TempDir(), "out.db")
	if _, _, err := store.DecompressBackup(context.Background(), trunc, out, 1<<20); err == nil {
		t.Fatal("truncated gzip must be rejected")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a failed decompress must not leave the output file behind")
	}
}

func TestDecompressRejectsWrongMagic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fake.db.gz")
	_ = os.WriteFile(p, []byte("this is not gzip at all, just text"), 0o600)
	out := filepath.Join(t.TempDir(), "out.db")
	_, _, err := store.DecompressBackup(context.Background(), p, out, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("err = %v, want a clear gzip error", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output must not exist after a rejected stream")
	}
}

func TestDecompressEnforcesSizeCap(t *testing.T) {
	// 50 MB of zeros compresses to ~50 KB: a classic bomb shape.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(make([]byte, 50<<20))
	_ = zw.Close()
	p := filepath.Join(t.TempDir(), "bomb.db.gz")
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	out := filepath.Join(t.TempDir(), "out.db")
	_, _, err := store.DecompressBackup(context.Background(), p, out, 1<<20)
	if !errors.Is(err, store.ErrBackupTooLarge) {
		t.Fatalf("err = %v, want ErrBackupTooLarge", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("over-cap output must be removed")
	}
	if fi, err := os.Stat(p); err != nil || fi.Size() > 1<<20 {
		t.Fatal("test setup: bomb should be small on disk")
	}
}

func TestSniffBackupKind(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want store.BackupKind
	}{
		{"gzip", []byte{0x1f, 0x8b, 8, 0}, store.BackupKindGzip},
		{"sqlite", []byte("SQLite format 3\x00rest"), store.BackupKindPlain},
		{"text", []byte("hello world, not a db"), store.BackupKindUnknown},
		{"short", []byte{1}, store.BackupKindUnknown},
	}
	for _, c := range cases {
		if got := store.SniffBackupKind(c.head); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestBackupToAcceptsPrecreatedEmptyFileAndReportsFootprint(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, wal, err := s.Footprint()
	if err != nil || db <= 0 || wal < 0 {
		t.Fatalf("Footprint = %d, %d, %v; want a positive db size", db, wal, err)
	}
	dst := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(dst, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.BackupTo(context.Background(), dst); err != nil {
		t.Fatalf("BackupTo into a pre-created empty file: %v", err)
	}
	if err := store.ValidateBackupFile(dst); err != nil {
		t.Fatalf("snapshot did not validate: %v", err)
	}
}

func TestFileFingerprintMatchesCompressFingerprint(t *testing.T) {
	payload := sqliteLike(70_000)
	_, fp := gzBackup(t, payload)
	p := filepath.Join(t.TempDir(), "p.db")
	_ = os.WriteFile(p, payload, 0o600)
	got, err := store.FileFingerprint(context.Background(), p)
	if err != nil || got != fp {
		t.Fatalf("FileFingerprint = %q, %v; want %q", got, err, fp)
	}
}

func TestFingerprintTailLayoutIsExactAndGunzipCompatible(t *testing.T) {
	payload := sqliteLike(9000)
	gz, fp := gzBackup(t, payload)
	data, _ := os.ReadFile(gz)

	tail := data[len(data)-99:]
	if tail[0] != 0x1f || tail[1] != 0x8b || tail[3] != 0x10 {
		t.Fatalf("tail does not start with a gzip member header: % x", tail[:10])
	}
	if !strings.Contains(string(tail), fp) {
		t.Fatal("tail does not carry the fingerprint")
	}

	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Error("gunzip of a backup does not give back the original database")
	}

	for name, mutate := range map[string]func([]byte) []byte{
		"flipped hex byte": func(b []byte) []byte { c := append([]byte(nil), b...); c[len(c)-30] ^= 1; return c },
		"trailing byte":    func(b []byte) []byte { return append(append([]byte(nil), b...), 0) },
	} {
		p := filepath.Join(t.TempDir(), "m.db.gz")
		_ = os.WriteFile(p, mutate(data), 0o600)
		if got, ok := store.ReadFingerprintTail(p); ok && got == fp {
			t.Errorf("%s: a damaged tail was accepted as the fingerprint", name)
		}
	}
}

func TestDecompressLimit(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name   string
		plan   store.DecompressPlan
		want   int64
		noRoom bool
	}{
		{"ratio cap from compressed size", store.DecompressPlan{FreeBytes: 1 << 40, MarginBytes: 128 * mib, CompressedBytes: 10 * mib, HardCap: 1 << 40}, 10 * mib * store.MaxBackupExpansionRatio, false},
		{"live db size floors the ratio cap", store.DecompressPlan{FreeBytes: 1 << 40, MarginBytes: 128 * mib, CompressedBytes: mib, LiveDBBytes: 900 * mib, HardCap: 1 << 40}, 900 * mib, false},
		{"free space minus margin lowers it", store.DecompressPlan{FreeBytes: 300 * mib, MarginBytes: 128 * mib, CompressedBytes: 100 * mib, HardCap: 1 << 40}, 172 * mib, false},
		{"hard cap lowers it", store.DecompressPlan{FreeBytes: 1 << 40, MarginBytes: 128 * mib, CompressedBytes: 100 * mib, HardCap: 512}, 512, false},
		{"no room is an error never a zero limit", store.DecompressPlan{FreeBytes: 100 * mib, MarginBytes: 128 * mib, CompressedBytes: mib, HardCap: 1 << 40}, 0, true},
	}
	for _, c := range cases {
		got, err := store.DecompressLimit(c.plan)
		var nr *store.NoRoomError
		if c.noRoom {
			if !errors.As(err, &nr) || got != 0 {
				t.Errorf("%s: got %d, %v; want a NoRoomError", c.name, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %d, %v; want %d", c.name, got, err, c.want)
		}
	}
}

func TestDecompressMaxInt64CapDoesNotOverflow(t *testing.T) {
	gz, fp := gzBackup(t, sqliteLike(50_000))
	out := filepath.Join(t.TempDir(), "out.db")
	gotFP, n, err := store.DecompressBackup(context.Background(), gz, out, 1<<63-1)
	if err != nil || n != 50_000 || gotFP != fp {
		t.Fatalf("DecompressBackup with a MaxInt64 cap = %d, %v; want a normal success", n, err)
	}
}

func TestDecompressRefusesTooManyMembers(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < store.MaxGzipMembers+4; i++ {
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte("x"))
		_ = zw.Close()
	}
	p := filepath.Join(t.TempDir(), "many.db.gz")
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	_, _, err := store.FingerprintGzipBackup(context.Background(), p, 1<<20)
	var bi *store.BackupInvalidError
	if !errors.As(err, &bi) {
		t.Fatalf("err = %v, want a BackupInvalidError for too many members", err)
	}
}

func TestDecompressStopsWhenContextCancelled(t *testing.T) {
	gz, _ := gzBackup(t, sqliteLike(2_000_000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(t.TempDir(), "out.db")
	if _, _, err := store.DecompressBackup(ctx, gz, out, 1<<30); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a cancelled decompress must remove its output")
	}
	if _, err := store.FileFingerprint(ctx, gz); !errors.Is(err, context.Canceled) {
		t.Errorf("FileFingerprint err = %v, want context.Canceled", err)
	}
}

func TestValidationErrorsCarryAPathFreeReason(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret-name.db")
	_ = os.WriteFile(p, []byte("not a database at all, just text that is long enough to be read"), 0o600)
	err := store.ValidateBackupFile(p)
	var bi *store.BackupInvalidError
	if !errors.As(err, &bi) {
		t.Fatalf("err = %v, want a *BackupInvalidError", err)
	}
	if strings.Contains(bi.Reason, dir) || strings.Contains(bi.Reason, "secret-name") {
		t.Errorf("Reason %q leaks the file path", bi.Reason)
	}
}

func TestFootprintReadsTheFileTheStoreOpened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.DBPath() != path {
		t.Fatalf("DBPath = %q, want %q", s.DBPath(), path)
	}
	db, _, err := s.Footprint()
	st, _ := os.Stat(path)
	if err != nil || db != st.Size() {
		t.Fatalf("Footprint db = %d, %v; want the size of the opened file (%d)", db, err, st.Size())
	}
}

// A budget that refuses part-way ends the decompression, hands the caller's own
// error back, and removes the partial output.
func TestDecompressBudgetRefusalStopsAndRemovesTheOutput(t *testing.T) {
	gz, _ := gzBackup(t, sqliteLike(3_000_000))
	errNoRoom := errors.New("the volume ran out of room")
	var asked int
	ensure := func(total int64) error {
		asked++
		if total > 1_000_000 {
			return errNoRoom
		}
		return nil
	}
	out := filepath.Join(t.TempDir(), "out.db")
	_, _, err := store.DecompressBackup(context.Background(), gz, out, 1<<30, store.WithBudget(ensure))
	if !errors.Is(err, errNoRoom) {
		t.Fatalf("err = %v, want the budget's refusal", err)
	}
	if asked == 0 {
		t.Fatal("the budget was never consulted")
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Error("a refused decompression left its partial output")
	}
}

// A snapshot honours its context: a cancelled one stops, and the store keeps
// working afterwards.
func TestBackupToHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.BackupTo(ctx, filepath.Join(dir, "snap.db")); err == nil {
		t.Fatal("a cancelled snapshot reported success")
	}
	if err := s.BackupTo(context.Background(), filepath.Join(dir, "snap2.db")); err != nil {
		t.Fatalf("the store did not recover from a cancelled snapshot: %v", err)
	}
}
