package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

var stdinIsTTY = func() bool { return isTerminal(os.Stdin.Fd()) }
var stdinReader io.Reader = os.Stdin

func requireConfirm(action, target string, yes bool, stderr io.Writer) error {
	if yes {
		return nil
	}
	if !stdinIsTTY() {
		return userErrorf("refusing to %s %q without --yes (no TTY to confirm)", action, target)
	}
	return promptProceed(stderr, fmt.Sprintf("This will %s %q. This action cannot be undone.", action, target))
}

// promptProceed prints message followed by "Proceed? [y/N]" on stderr and
// reads one answer from stdin. It returns nil only for y or yes. The caller
// has already checked that stdin is a terminal.
func promptProceed(stderr io.Writer, message string) error {
	fmt.Fprintf(stderr, "%s\nProceed? [y/N] ", message)
	br := bufio.NewReader(stdinReader)
	line, err := br.ReadString('\n')
	if err != nil && len(line) == 0 {
		return userErrorf("aborted: could not read confirmation")
	}
	ans := strings.TrimSpace(strings.ToLower(line))
	if ans == "y" || ans == "yes" {
		return nil
	}
	return userErrorf("aborted")
}
