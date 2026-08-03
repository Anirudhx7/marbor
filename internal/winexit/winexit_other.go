//go:build !windows

package winexit

import "os"

// Exit terminates the process with code. No console pause outside Windows.
func Exit(code int) {
	os.Exit(code)
}
