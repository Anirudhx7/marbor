//go:build windows

package main

import (
	"errors"
	"syscall"
)

// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION, which
// the standard syscall package does not export.
const processQueryLimitedInformation = 0x1000

// pidAlive reports whether a process with the given pid currently exists.
// Windows has no signal-0 probe, so it opens a handle to the process and
// checks whether that handle is signaled (a process handle becomes signaled
// when the process exits).
func pidAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return pidAliveFromOpenErr(err)
	}
	defer syscall.CloseHandle(h)

	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		// Could not tell; assume alive so a caller waiting for exit never
		// reports a stop that did not happen.
		return true
	}
	return event == syscall.WAIT_TIMEOUT
}

// pidAliveFromOpenErr classifies an OpenProcess failure. Access denied means
// the process exists but belongs to someone we cannot open, so it counts as
// alive. Any other failure (typically an invalid parameter for a pid that
// does not exist) counts as dead.
func pidAliveFromOpenErr(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
