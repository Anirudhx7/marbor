//go:build !linux && !darwin && !windows

// Fallback for any GOOS this package doesn't have a service-manager backend
// for yet (e.g. freebsd). New() itself is defined once per platform (see
// service_linux.go/service_darwin.go/service_windows.go and service.go's
// New() doc comment) - this file is the "none of the above" branch,
// returning a clear error rather than a partial/best-effort Manager.
package service

import (
	"fmt"
	"runtime"
)

// New reports that this GOOS has no supported service-manager backend. Per
// the node-agent design doc: promise the architecture (any OS
// can get a Manager implementation later), not universal day-one coverage.
func New() (Manager, error) {
	return nil, fmt.Errorf("service: no supported service manager for GOOS %q - install and run %q manually in the foreground, or in your own init system", runtime.GOOS, Name)
}
