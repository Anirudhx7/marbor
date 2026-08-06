package cli

import (
	"bufio"
	"os"
	"strings"
)

// readPassword reads one line from stdin with terminal echo disabled, for
// interactive "login" - the password is never displayed while typed. If
// stdin isn't a real terminal (piped/redirected input, e.g. in a script or a
// test), disableEcho returns an error and this falls back to a plain read -
// there is no terminal mode to disable, and failing the whole command over
// it would break non-interactive use for no reason.
func readPassword(stdin *os.File) (string, error) {
	restore, err := disableEcho(stdin.Fd())
	if err != nil {
		return readLine(stdin)
	}
	defer restore()
	return readLine(stdin)
}

func readLine(f *os.File) (string, error) {
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
