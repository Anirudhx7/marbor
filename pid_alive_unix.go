//go:build !windows

package main

import (
	"os"
	"syscall"
)

// pidAlive reports whether a process with the given pid currently exists,
// using a signal-0 probe (no signal is delivered; only existence and
// permission are checked).
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return signalProbeMeansAlive(proc.Signal(syscall.Signal(0)))
}
