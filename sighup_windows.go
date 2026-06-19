//go:build windows

package main

import "os"

func setupReloadSignal(_ chan<- os.Signal) {}
