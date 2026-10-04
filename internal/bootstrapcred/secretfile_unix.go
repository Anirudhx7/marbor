//go:build unix

package bootstrapcred

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openNoFollow makes open fail instead of following a symlink at the path.
const openNoFollow = syscall.O_NOFOLLOW

// openNonBlock keeps an open of a named pipe from blocking.
const openNonBlock = syscall.O_NONBLOCK

// hasSingleLink reports whether the opened file has exactly one hard link.
func hasSingleLink(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Nlink) == 1
}

// restrictFile sets the mode to 0600 explicitly, so a restrictive or loose
// umask at creation time cannot change what the file ends up as.
func restrictFile(f *os.File, _ string) error { return f.Chmod(0o600) }

// checkOwnerAndMode requires owner-only permissions (0600 or stricter, such as
// 0400), a single hard link, and ownership by the current user. It runs on
// the already-opened descriptor's Stat.
func checkOwnerAndMode(path string, info fs.FileInfo) error {
	if perm := info.Mode().Perm(); perm&^0o600 != 0 {
		return fmt.Errorf("%s has mode %04o, want 0600 or stricter", path, perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine owner", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the service user (uid %d)", path, st.Uid, os.Geteuid())
	}
	if !hasSingleLink(info) {
		return fmt.Errorf("%s has %d hard links, want exactly 1", path, uint64(st.Nlink))
	}
	return nil
}
