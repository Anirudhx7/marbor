//go:build windows

package admin

import (
	"errors"
	"syscall"
)

// openNoFollow is zero on Windows: there is no O_NOFOLLOW. openBackupFile
// instead compares the opened handle with the Lstat result (os.SameFile), which
// catches a file swapped in between the check and the open.
const openNoFollow = 0

const (
	errorHandleDiskFull syscall.Errno = 39  // ERROR_HANDLE_DISK_FULL
	errorDiskFull       syscall.Errno = 112 // ERROR_DISK_FULL
)

// isDiskFull reports an out-of-space write error.
func isDiskFull(err error) bool {
	return errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull)
}
