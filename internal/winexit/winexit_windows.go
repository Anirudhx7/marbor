//go:build windows

package winexit

import (
	"bufio"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetConsoleProcessList isn't wrapped by golang.org/x/sys/windows, so it's
// resolved directly from kernel32.dll, the same lazy-load pattern x/sys
// itself uses internally for less common syscalls.
var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// getConsoleProcessList wraps the Win32 GetConsoleProcessList call: it fills
// pids with up to len(pids) process IDs attached to the calling process's
// console and returns the total number of processes attached (which can
// exceed len(pids) if the buffer was too small - irrelevant here since we
// only care whether that total is more than one).
func getConsoleProcessList(pids []uint32) (uint32, error) {
	r, _, err := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	if r == 0 {
		return 0, err
	}
	return uint32(r), nil
}

// Exit terminates the process with code. On a non-zero code, if this
// process owns its own console (GetConsoleProcessList reports only this
// process attached), it waits for a keypress first - otherwise the console
// window closes before a double-click-launched user can read the error.
// A pre-existing shell (an already-open terminal) always has more than one
// process attached to the console, so it never pauses in that case.
func Exit(code int) {
	if code != 0 && ownsConsole() {
		fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

func ownsConsole() bool {
	var pids [1]uint32
	n, err := getConsoleProcessList(pids[:])
	return err == nil && n <= 1
}
