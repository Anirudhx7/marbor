//go:build unix

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// disableEcho turns off terminal input echo for fd via termios, returning a
// restore func. ECHO is cleared but ICANON is left set, so line editing
// (backspace, etc.) still works via the kernel's terminal driver - only
// character display is suppressed, same behavior as a shell's `read -s`.
// golang.org/x/sys/unix is already part of this module's existing
// golang.org/x/sys dependency (same module as the windows subpackage used
// elsewhere in the repo), so this adds no new dependency.
func disableEcho(fd uintptr) (restore func(), err error) {
	termios, err := unix.IoctlGetTermios(int(fd), ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	original := *termios
	newState := *termios
	newState.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(fd), ioctlWriteTermios, &newState); err != nil {
		return nil, err
	}
	return func() {
		if err := unix.IoctlSetTermios(int(fd), ioctlWriteTermios, &original); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore terminal echo: %v (run \"stty echo\" or start a new shell if input stops echoing)\n", err)
		}
	}, nil
}

// isTerminal reports whether fd is a real terminal, used to decide whether
// "login" should prompt interactively at all (piped/redirected input has no
// terminal to disable echo on).
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), ioctlReadTermios)
	return err == nil
}
