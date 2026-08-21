package control

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached and findProcess are seams (same pattern as lookPath/
// runCommand) so ProcessDriver's tests can simulate process launch/lookup
// without actually spawning or signaling a real OS process.
var startDetached = func(name string, args ...string) (*os.Process, error) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

var findProcess = os.FindProcess

// processAlive reports whether pid names a live process. On Unix,
// os.FindProcess always succeeds regardless of whether the pid exists, so
// liveness is confirmed by sending signal 0 (the standard no-op existence
// probe). On Windows, os.FindProcess itself opens a process handle and
// fails if the pid doesn't exist, so a successful FindProcess is already
// sufficient - Signal(0) isn't supported there for arbitrary processes, so
// an "not supported" response from Signal is treated as "alive" rather than
// a failure, since FindProcess having succeeded already answered the
// question.
var processAlive = func(pid int) bool {
	proc, err := findProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return isUnsupportedSignal(err)
}

func isUnsupportedSignal(err error) bool {
	return err != nil && err.Error() == "not supported by windows"
}
