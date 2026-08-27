package cli

import (
	"bufio"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
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

	// P291: echo restoration otherwise only happens via the deferred
	// restore() below, which a fatal signal/Ctrl+C during the blocked
	// ReadString call skips entirely - neither the kernel (Unix) nor
	// Windows reverts console echo mode automatically once the owning
	// process dies, so the terminal is left silently echo-disabled for
	// whatever runs next in that same session. Install a synchronous
	// interrupt handler that restores the terminal state before the
	// process actually exits. os.Process.Signal only supports re-raising
	// os.Kill on Windows (os.Interrupt returns "not supported"), so this
	// exits directly with the conventional SIGINT exit code (130) rather
	// than trying to re-raise the signal - identical behavior on both
	// platforms.
	var once sync.Once
	safeRestore := func() { once.Do(restore) }
	// returned guards against a race (caught by code review) where a
	// signal lands in sigCh at essentially the same instant readLine
	// already returned successfully - select picks pseudo-randomly among
	// ready cases, so without this flag the goroutine below could still
	// take the sigCh branch and os.Exit(130) after a successful read,
	// discarding a password the caller already has. Set before signaling
	// done, so the goroutine can check it before hard-exiting.
	var returned atomic.Bool
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			if returned.Load() {
				return
			}
			safeRestore()
			os.Exit(130)
		case <-done:
		}
	}()
	line, err := readLine(r)
	returned.Store(true)
	close(done)
	signal.Stop(sigCh)
	safeRestore()
	return line, err
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
