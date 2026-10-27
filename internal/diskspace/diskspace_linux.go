//go:build linux

package diskspace

import (
	"fmt"
	"syscall"
)

// stat reads the volume counters. On Linux f_bavail and f_blocks are counted
// in units of f_frsize (the fragment size), not f_bsize (the preferred I/O
// size), so Frsize is the multiplier; Bsize is only a fallback for a kernel or
// filesystem that leaves Frsize zero.
func stat(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	unit := uint64(st.Frsize) //nolint:unconvert // field widths differ per arch
	if unit == 0 {
		unit = uint64(st.Bsize) //nolint:unconvert // field widths differ per arch
	}
	return Usage{
		Free:  uint64(st.Bavail) * unit, //nolint:unconvert // field widths differ per arch
		Total: uint64(st.Blocks) * unit, //nolint:unconvert // field widths differ per arch
	}, nil
}
