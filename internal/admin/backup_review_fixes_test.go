package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A gzip stream that is intact but has no fingerprint record (made by the gzip
// tool) restores fine, so verification accepts it and computes the fingerprint.
func TestVerifyBackupFile_TaillessIntactGzipIsVerified(t *testing.T) {
	s, dir := newServerWithDir(t)
	path := filepath.Join(dir, backupFilename(time.Now().Add(-time.Hour)))
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(validSQLiteBytes(t)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := s.verifyBackupFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if e.reason != "" || e.fp == "" || e.raw == 0 {
		t.Fatalf("entry %+v, want a verified entry with a computed fingerprint", e)
	}
}

// A plain backup with a good header but damaged pages is not a good backup, so
// retention does not count it and the older good copy is kept.
func TestPrune_CorruptPlainFileIsNotCounted(t *testing.T) {
	s, _ := newBackupTestServer(t)
	dir := t.TempDir()
	valid := validSQLiteBytes(t)
	ts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	good := backupFilename(ts)
	writeGz(t, dir, good, valid)
	damagedName := legacyBackupFilename(ts.Add(time.Hour))
	damaged := append([]byte{}, valid...)
	for i := 100; i < len(damaged); i++ {
		damaged[i] = 0xAB
	}
	if err := os.WriteFile(filepath.Join(dir, damagedName), damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := s.verifyBackupFile(context.Background(), filepath.Join(dir, damagedName))
	if err != nil || e.reason == "" {
		t.Fatalf("damaged plain file verified as good: entry %+v err %v", e, err)
	}
	if problem := s.pruneBackups(context.Background(), dir, 1, false); !strings.Contains(problem, "damaged") {
		t.Fatalf("prune must report the damaged file, got %q", problem)
	}
	got := backupFilesIn(t, dir)
	if len(got) != 2 {
		t.Fatalf("after prune: %v, want the good copy and the damaged file both kept", got)
	}
}

// A hold that is extended on every call does not expire while a long write is
// alive, even when it already covers the size asked for.
func TestGrowingHold_EnsureRefreshesTheExpiry(t *testing.T) {
	s, dir := newServerWithDir(t)
	hold, err := s.newGrowingHold(dir, 1<<20, 1<<30, "the test was running")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.release()
	s.backupSpace.mu.Lock()
	hold.hold.expires = time.Now().Add(time.Minute)
	s.backupSpace.mu.Unlock()
	if err := hold.ensure(1 << 10); err != nil {
		t.Fatal(err)
	}
	s.backupSpace.mu.Lock()
	left := time.Until(hold.hold.expires)
	s.backupSpace.mu.Unlock()
	if left < holdTTL/2 {
		t.Fatalf("expiry left = %v, want it pushed out to about %v", left, holdTTL)
	}
}

type failingClockReader struct {
	clk *time.Time
	per time.Duration
}

func (r failingClockReader) Read(p []byte) (int, error) {
	*r.clk = r.clk.Add(r.per)
	return 0, errors.New("connection reset")
}

// Time spent in a read that ends in an error still counts toward the total.
func TestRateGuard_ErroredReadStillCountsItsTime(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := &rateGuard{r: failingClockReader{clk: &clk, per: 3 * time.Second}, now: func() time.Time { return clk }, minRate: 1, window: time.Hour, limit: time.Hour}
	if _, err := g.Read(make([]byte, 16)); err == nil {
		t.Fatal("expected the read error")
	}
	if g.total != 3*time.Second {
		t.Fatalf("total = %v, want 3s", g.total)
	}
}
