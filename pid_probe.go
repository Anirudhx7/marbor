package main

import (
	"errors"
	"syscall"
)

// signalProbeMeansAlive interprets the result of a signal-0 probe. A nil error
// means the process exists. EPERM means it exists but the caller may not
// signal it (a non-root marbor probing a root-owned process), which is still
// proof of life; only other errors (ESRCH, process already finished) mean it
// is gone.
func signalProbeMeansAlive(err error) bool {
	return err == nil || errors.Is(err, syscall.EPERM)
}
