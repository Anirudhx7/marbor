//go:build darwin || freebsd || netbsd || openbsd

package cli

import "golang.org/x/sys/unix"

// The BSD family (including Darwin/macOS) uses different termios ioctl
// requests than Linux (see termpw_ioctl_linux.go).
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
