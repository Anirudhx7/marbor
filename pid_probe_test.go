package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestSignalProbeMeansAlive(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"exists", nil, true},
		{"exists but not permitted", syscall.EPERM, true},
		{"wrapped permission error", &os.SyscallError{Syscall: "kill", Err: syscall.EPERM}, true},
		{"no such process", syscall.ESRCH, false},
		{"process finished", os.ErrProcessDone, false},
		{"other error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := signalProbeMeansAlive(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
