//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func setupReloadSignal(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
