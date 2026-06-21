//go:build windows

package admin

import (
	"syscall"
	"unsafe"
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readSystemMemory() (totalMB, freeMB int64) {
	var stat memoryStatusEx
	stat.Length = uint32(unsafe.Sizeof(stat))
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	globalMemoryStatusEx := kernel32.NewProc("GlobalMemoryStatusEx")
	r, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&stat)))
	if r != 0 {
		return int64(stat.TotalPhys / (1024 * 1024)), int64(stat.AvailPhys / (1024 * 1024))
	}
	return 0, 0
}
