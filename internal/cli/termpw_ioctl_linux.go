//go:build linux

package cli

import "golang.org/x/sys/unix"

// Linux's termios ioctl requests differ from the BSD family's (see
// termpw_ioctl_bsd.go) - golang.org/x/term uses this same split for the
// same reason.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
