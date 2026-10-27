package diskspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFreeAndSameVolume(t *testing.T) {
	dir := t.TempDir()
	free, err := Free(dir)
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if free == 0 {
		t.Fatal("Free reported 0 bytes on a temp dir with room to write")
	}
	same, err := SameVolume(dir, dir)
	if err != nil || !same {
		t.Fatalf("SameVolume(dir, dir) = %v, %v; want true, nil", same, err)
	}
	if _, err := Free(filepath.Join(dir, "does-not-exist-xyz")); err == nil {
		t.Error("Free on a missing path should error")
	}
}

func TestStatReportsTotalAtLeastFree(t *testing.T) {
	u, err := Stat(t.TempDir())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if u.Total == 0 || u.Free > u.Total {
		t.Fatalf("Stat = %+v; want a non-zero total and free <= total", u)
	}
}

func TestSameVolumeSubdirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	same, err := SameVolume(dir, sub)
	if err != nil || !same {
		t.Fatalf("SameVolume(dir, subdir) = %v, %v; want true, nil", same, err)
	}
}
