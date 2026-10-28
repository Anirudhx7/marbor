//go:build windows

package diskspace

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// stat uses GetDiskFreeSpaceEx through golang.org/x/sys/windows, which resolves
// kernel32 from the system directory only (never the application directory or
// the current directory), so a planted DLL cannot be loaded in its place.
func stat(path string) (Usage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return Usage{}, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", path, err)
	}
	return Usage{Free: avail, Total: total}, nil
}

// volumeID resolves path to a stable volume identity: the volume GUID path
// (\\?\Volume{...}\) of the volume it lives on. That is correct for an NTFS
// mount point folder as well as a drive letter, which the bare drive letter
// from filepath.VolumeName is not. When the volume has no GUID (a mapped
// network drive, for instance) the volume root path is the best identity
// available.
func volumeID(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return "", err
	}
	root := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &root[0], uint32(len(root))); err != nil {
		return "", fmt.Errorf("GetVolumePathName %s: %w", path, err)
	}
	rootStr := windows.UTF16ToString(root)
	guid := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeNameForVolumeMountPoint(&root[0], &guid[0], uint32(len(guid))); err != nil {
		return strings.ToLower(rootStr), nil
	}
	return strings.ToLower(windows.UTF16ToString(guid)), nil
}

// SameVolume reports whether a and b are on the same volume. An error means the
// answer is unknown; callers should then assume they share a volume.
func SameVolume(a, b string) (bool, error) {
	ia, err := volumeID(a)
	if err != nil {
		return false, err
	}
	ib, err := volumeID(b)
	if err != nil {
		return false, err
	}
	return ia == ib, nil
}
