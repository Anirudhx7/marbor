//go:build windows

package main

import (
	"syscall"
	"testing"
)

func TestPidAliveFromOpenErr(t *testing.T) {
	if !pidAliveFromOpenErr(syscall.ERROR_ACCESS_DENIED) {
		t.Error("access denied should count as alive")
	}
	// 87 is ERROR_INVALID_PARAMETER, which OpenProcess returns for a pid that
	// does not exist; the syscall package does not export a name for it.
	if pidAliveFromOpenErr(syscall.Errno(87)) {
		t.Error("invalid parameter (no such pid) should count as dead")
	}
}
