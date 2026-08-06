package cli

import (
	"bufio"
	"strings"
)

// readPassword reads one line from r (wrapping stdin) with terminal echo
// disabled on stdinFd, for interactive "login" - the password is never
// displayed while typed. If stdin isn't a real terminal (piped/redirected
// input, e.g. in a script or a test), disableEcho returns an error and this
// falls back to a plain read - there is no terminal mode to disable, and
// failing the whole command over it would break non-interactive use for no
// reason.
//
// r must be the same *bufio.Reader the caller used to read the preceding
// username line, not a fresh one constructed here: a bufio.Reader can read
// ahead into its internal buffer on a single underlying Read syscall (e.g.
// if a paste or an automated test delivers "username\npassword\n" in one
// burst before either line is consumed), so a second, independently
// constructed reader would never see bytes already buffered inside the
// first one - it would block waiting for input that had, in fact, already
// arrived.
func readPassword(stdinFd uintptr, r *bufio.Reader) (string, error) {
	restore, err := disableEcho(stdinFd)
	if err != nil {
		return readLine(r)
	}
	defer restore()
	return readLine(r)
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
