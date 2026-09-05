package control

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
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
	// Reap the child once it exits: the long-lived marbor-agent daemon
	// is this process's parent, and nothing else ever calls Wait() on it -
	// every Start/Restart cycle would otherwise leak a zombie once the child
	// exits. The result is intentionally discarded; the caller tracks the
	// runtime's liveness via its own PID file / processAlive, not via this
	// goroutine.
	go func() { _ = cmd.Wait() }()
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
	if runtime.GOOS == "windows" {
		// os.FindProcess itself opens a process handle and fails if the pid
		// doesn't exist, so a successful FindProcess already answers the
		// question - Signal(0) isn't meaningfully supported for arbitrary
		// processes on Windows anyway, so skip the probe entirely rather
		// than depend on matching its exact "not supported by windows"
		// error string, an implementation detail, not a stable contract.
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the pid exists but the caller lacks permission to signal it
	// (e.g. a non-root agent probing a root-owned runtime's PID file) - that
	// is proof of life, not death.
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.EPERM {
		return true
	}
	return false
}
