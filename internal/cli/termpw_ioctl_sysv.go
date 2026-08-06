//go:build solaris || illumos || aix

package cli

import "golang.org/x/sys/unix"

// Solaris/illumos/AIX use the same TCGETS/TCSETS ioctl requests as Linux
// (verified against golang.org/x/sys/unix for each GOOS - unlike the BSD
// family, see termpw_ioctl_bsd.go), just not the SAME build-constraint
// group as termpw_ioctl_linux.go.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
