package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

func newSnapshotBytes(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "snap.db")
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A staged database replaces the live file, the old (empty) WAL and SHM
// sidecars are gone, and the staged name no longer exists.
func TestSwapStagedRestore_ReplacesLiveAndRemovesSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := dbPath + ".restoring-aaaa"
	raw := newSnapshotBytes(t)
	if err := os.WriteFile(dbPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A clean close leaves an empty log; the shm is scratch.
	if err := os.WriteFile(dbPath+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-shm", []byte("shm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := swapStagedRestore(dbPath, staged); err != nil {
		t.Fatalf("swapStagedRestore = %v", err)
	}
	if got, _ := os.ReadFile(dbPath); !bytes.Equal(got, raw) {
		t.Error("live database does not hold the staged bytes")
	}
	for _, f := range []string{staged, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s should be gone after the swap (stat err = %v)", filepath.Base(f), err)
		}
	}
}

// A staged file that is no longer a database aborts the swap, is removed, and
// leaves the live database and its sidecars alone.
func TestSwapStagedRestore_RefusesDamagedStageAndKeepsLive(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := dbPath + ".restoring-bbbb"
	live := []byte("live database bytes that must survive")
	if err := os.WriteFile(dbPath, live, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("this is not a sqlite database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := swapStagedRestore(dbPath, staged); err == nil {
		t.Fatal("a damaged staged file must not be swapped in")
	}
	if got, _ := os.ReadFile(dbPath); !bytes.Equal(got, live) {
		t.Error("live database was modified by a refused swap")
	}
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Error("the live WAL sidecar must survive a refused swap")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("refused swap left the staged file behind: %v", err)
	}
}

// A header-valid file that fails the full integrity/schema check is refused
// after the drain, even though the earlier header check would pass it.
func TestSwapStagedRestore_FullValidationAfterDrain(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := dbPath + ".restoring-cccc"
	raw := newSnapshotBytes(t)
	// Keep the 100-byte header, destroy the pages behind it.
	bad := append([]byte{}, raw...)
	for i := 100; i < len(bad); i++ {
		bad[i] = 0xAB
	}
	if err := os.WriteFile(dbPath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := swapStagedRestore(dbPath, staged); err == nil {
		t.Fatal("a staged file with corrupt pages must not be swapped in")
	}
	if got, _ := os.ReadFile(dbPath); string(got) != "live" {
		t.Error("live database changed")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("refused swap left the staged file behind: %v", err)
	}
}

// A non-empty old write-ahead log means the old database did not close cleanly;
// replacing the file would replay that log against the new one, so refuse.
func TestSwapStagedRestore_RefusesNonEmptyOldWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := dbPath + ".restoring-dddd"
	if err := os.WriteFile(dbPath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("unflushed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, newSnapshotBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := swapStagedRestore(dbPath, staged)
	if err == nil || !strings.Contains(err.Error(), "write-ahead") {
		t.Fatalf("want a write-ahead refusal, got %v", err)
	}
	if got, _ := os.ReadFile(dbPath); string(got) != "live" {
		t.Error("live database changed")
	}
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Error("the live WAL must survive a refused swap")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("refused swap left the staged file behind: %v", err)
	}
}

// A missing staged file is an error, never a silent success.
func TestSwapStagedRestore_MissingStageFails(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	if err := os.WriteFile(dbPath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := swapStagedRestore(dbPath, dbPath+".restoring-eeee"); err == nil {
		t.Fatal("missing staged file must fail")
	}
	if got, _ := os.ReadFile(dbPath); string(got) != "live" {
		t.Error("live database changed")
	}
}

// The swap always validates the staged file in full: a file with a valid header
// but damaged pages is refused and the live database is left alone, as is a file
// without a SQLite header.
func TestSwapStagedRestore_RefusesDamagedStage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := dbPath + ".restoring-ffff"
	damaged := append([]byte{}, newSnapshotBytes(t)...)
	for i := 100; i < len(damaged); i++ {
		damaged[i] = 0xAB
	}
	if err := os.WriteFile(dbPath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := swapStagedRestore(dbPath, staged); err == nil {
		t.Fatal("a staged file with damaged pages was swapped in")
	}
	if got, _ := os.ReadFile(dbPath); string(got) != "live" {
		t.Error("live database changed by a refused swap")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the refused staged file was not removed")
	}

	staged2 := dbPath + ".restoring-gggg"
	if err := os.WriteFile(staged2, []byte("not a database at all, just text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := swapStagedRestore(dbPath, staged2); err == nil {
		t.Fatal("a staged file without a SQLite header was accepted")
	}
	if got, _ := os.ReadFile(dbPath); string(got) != "live" {
		t.Error("live database changed by a refused swap")
	}
}

// A symbolic link put in place of the staged file is refused: the rename would
// otherwise make the link, not a validated file, the database.
func TestSwapStagedRestore_RefusesSymlinkStage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "marbor.db")
	staged := dbPath + ".restoring-hhhh"
	target := filepath.Join(dir, "elsewhere.db")
	if err := os.WriteFile(target, newSnapshotBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, staged); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if _, err := swapStagedRestore(dbPath, staged); err == nil {
		t.Fatal("a symbolic link was accepted as the staged file")
	}
	if got, _ := os.ReadFile(dbPath); string(got) != "live" {
		t.Error("live database changed by a refused swap")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the link target must not be removed: %v", err)
	}
}
