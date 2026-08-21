//go:build windows

package cli

import "golang.org/x/sys/windows"

// disableEcho turns off console input echo for fd (the console handle
// backing stdin), returning a restore func. golang.org/x/sys/windows is
// already a direct dependency of this module (used by internal/winexit and
// internal/marboragent's Windows service code), so this adds no new
// dependency.
func disableEcho(fd uintptr) (restore func(), err error) {
	h := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	newMode := mode &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(h, newMode); err != nil {
		return nil, err
	}
	return func() { windows.SetConsoleMode(h, mode) }, nil
}

// isTerminal reports whether fd is a real console, used to decide whether
// "login" should prompt interactively at all (piped/redirected input has no
// terminal to disable echo on).
func isTerminal(fd uintptr) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(fd), &mode) == nil
}
