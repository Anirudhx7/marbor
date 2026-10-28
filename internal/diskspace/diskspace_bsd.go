//go:build !windows && !linux

package diskspace

import (
	"fmt"
	"syscall"
)

// stat reads the volume counters. On the BSDs and macOS f_bavail and f_blocks
// are counted in units of f_bsize.
func stat(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	unit := uint64(st.Bsize) //nolint:unconvert // field widths differ per OS
	return Usage{
		Free:  uint64(st.Bavail) * unit, //nolint:unconvert // field widths differ per OS
		Total: uint64(st.Blocks) * unit, //nolint:unconvert // field widths differ per OS
	}, nil
}
