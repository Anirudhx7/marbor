//go:build !windows

package admin

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow makes open fail on a symlink instead of following it.
const openNoFollow = syscall.O_NOFOLLOW

// isDiskFull reports an out-of-space or over-quota write error.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// dirOwnerUID returns the user that owns the directory fi describes.
func dirOwnerUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// processUID is the user marbor runs as.
func processUID() uint32 { return uint32(os.Geteuid()) }
