//go:build unix

package bootstrapcred

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWriteNew_ModeIsExplicitUnderAnyUmask(t *testing.T) {
	for _, umask := range []int{0o000, 0o277, 0o077} {
		old := syscall.Umask(umask)
		path := filepath.Join(t.TempDir(), FileName)

		err := WriteNew(path, "secret-value-123")
		syscall.Umask(old)

		if err != nil {
			t.Fatalf("umask %04o: WriteNew: %v", umask, err)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("umask %04o: mode = %v, want 0600", umask, info.Mode().Perm())
		}
		if _, err := ReadSafe(path); err != nil {
			t.Errorf("umask %04o: a file this package just wrote must verify: %v", umask, err)
		}
	}
}

func TestVerifySafe_AcceptsOwnerReadOnlyRejectsLooseAndLinked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := WriteNew(path, "secret-value-123"); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := VerifySafe(path); err != nil {
		t.Errorf("0400 must be accepted: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := VerifySafe(path); err == nil {
		t.Error("0640 must be refused")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "second-name")
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := VerifySafe(path); err == nil {
		t.Error("a file with a second hard link must be refused")
	}
}

func TestResolveSecret_NamedPipeDoesNotBlockAndIsRefused(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)

	go func() {
		_, _, err := ResolveSecret(func(k string) string {
			if k == EnvPasswordFile {
				return fifo
			}
			return ""
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a named pipe must be refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a named pipe blocked start-up")
	}
}

func TestRemove_DoesNotZeroAHardLinkedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	other := filepath.Join(dir, "other-name")
	if err := os.WriteFile(other, []byte("precious content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, path); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("the name must be unlinked")
	}
	got, err := os.ReadFile(other)
	if err != nil || string(got) != "precious content\n" {
		t.Errorf("the other link was overwritten: %q (err %v)", got, err)
	}
}

func TestRemove_ZeroesASingleLinkFileBeforeUnlinking(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := WriteNew(path, "secret-value-123"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The open descriptor still reads the unlinked inode: it must hold zeros.
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	for _, b := range buf[:n] {
		if b != 0 {
			t.Fatalf("file content was not zeroed before unlinking: %q", buf[:n])
		}
	}
}
