package bootstrapcred

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// maxSecretFileBytes bounds how much of a password file is ever read.
const maxSecretFileBytes = 4096

// ErrEmptySecretFile is wrapped by ReadSafe when the file exists, is safe, and
// holds no password. A crash between creating the file and writing the secret
// leaves exactly this state, so the caller may remove it and start again.
var ErrEmptySecretFile = errors.New("the file is empty")

// FilePath returns the generated-password file path inside dataDir.
func FilePath(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// WriteNew creates the password file exclusively (it must not exist), refusing
// symlinks, with owner-only permissions, then flushes it to disk. The file is
// created empty, restricted to the owner, and only then does the password (plus
// a newline) go in, so the secret never sits in a file others can read. Any
// failure removes the file again and is returned: nothing is skipped silently.
func WriteNew(path, password string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|openNoFollow, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	fail := func(step string, err error) error {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("%s %s: %w", step, path, err)
	}
	if err := restrictFile(f, path); err != nil {
		return fail("restrict", err)
	}
	if _, err := io.WriteString(f, password+"\n"); err != nil {
		return fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// Exists reports whether anything (including a symlink) sits at path.
func Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// openSafe opens path for reading and validates the OPENED descriptor, so there
// is no gap between the check and the use: not a symlink (O_NOFOLLOW where the
// platform has it, plus a same-file check against the pre-open Lstat), a
// regular file, owned by the current user, private to it, and on unix not
// hard-linked elsewhere. The caller closes the file.
func openSafe(path string) (*os.File, error) {
	pre, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if pre.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if !os.SameFile(pre, info) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", path)
	}
	if err := checkOwnerAndMode(path, info); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// VerifySafe checks that path is a regular file (not a symlink), owned by the
// current user and private to it. The error names the path and the problem.
func VerifySafe(path string) error {
	f, err := openSafe(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// ReadSafe verifies the file and returns its password (trailing newline
// removed), reading from the same descriptor that was verified. An empty file
// returns an error wrapping ErrEmptySecretFile.
func ReadSafe(path string) (string, error) {
	f, err := openSafe(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readPassword(f, path)
}

// readPasswordFile reads an operator-supplied password file, trimming trailing
// line endings. Symlinks are followed (Docker and Kubernetes secrets are
// symlinks), but the target must be a regular file: the open is non-blocking so
// a named pipe cannot hang start-up, and a pipe, device or directory is refused
// after fstat on the opened descriptor.
func readPasswordFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	return readPassword(f, path)
}

// readPassword reads the secret from an opened password file and trims trailing
// \r and \n only; both the generated-file and the operator-file paths share this
// one trim. It deliberately does not apply the password policy: the generated
// file is only ever written by this package from Generate (24 characters), the
// stored hash is the real check, and a policy rejection here could only turn a
// harmless edit such as a trailing blank into a lockout of the install. The
// operator-supplied secret is policy-checked by ResolveSecret.
func readPassword(f *os.File, path string) (string, error) {
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxSecretFileBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxSecretFileBytes)
	}
	pw := strings.TrimRight(string(b), "\r\n")
	if pw == "" {
		return "", fmt.Errorf("%s: %w", path, ErrEmptySecretFile)
	}
	return pw, nil
}

// Remove overwrites the file with zeros, flushes, and unlinks it. It is best
// effort: journaling and copy-on-write filesystems can keep old blocks. A
// missing file is not an error. A failed scrub is logged (path only) and the
// unlink still happens. The error names the path only.
func Remove(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode().IsRegular() {
		if err := overwriteZeros(path, info); err != nil {
			log.Printf("WARNING: could not overwrite %s before removing it: %v", path, err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// overwriteZeros is the best-effort scrub step of Remove. It zeroes only a file
// that is provably this one: the opened descriptor must be a regular file, the
// same file the earlier Lstat saw, and (where the platform reports it) have a
// single hard link. A hard-linked file may be another file's content, so it is
// left alone and Remove just unlinks the name.
func overwriteZeros(path string, pre fs.FileInfo) error {
	f, err := os.OpenFile(path, os.O_WRONLY|openNoFollow|openNonBlock, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(pre, info) {
		return fmt.Errorf("%s changed before it could be scrubbed", path)
	}
	if !hasSingleLink(info) {
		return fmt.Errorf("%s has other hard links, so it is unlinked without being overwritten", path)
	}
	if info.Size() > 0 {
		if _, err := f.Write(make([]byte, info.Size())); err != nil {
			return err
		}
	}
	return f.Sync()
}
