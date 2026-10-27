//go:build !windows

package diskspace

import (
	"fmt"
	"os"
	"syscall"
)

// SameVolume reports whether a and b are on the same filesystem device. An
// error means the answer is unknown; callers should then assume they share a
// volume.
func SameVolume(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	sa, okA := ia.Sys().(*syscall.Stat_t)
	sb, okB := ib.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return false, fmt.Errorf("no device id available for %s / %s", a, b)
	}
	return uint64(sa.Dev) == uint64(sb.Dev), nil //nolint:unconvert // Dev width differs per OS
}
