//go:build !windows

package admin

import (
	"errors"
	"syscall"
)

// openNoFollow makes open fail on a symlink instead of following it.
const openNoFollow = syscall.O_NOFOLLOW

// isDiskFull reports an out-of-space or over-quota write error.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
