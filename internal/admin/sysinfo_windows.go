//go:build windows

package admin

import (
	"log"
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

func readSystemMemory() (totalMB, freeMB int64, ok bool) {
	var stat memoryStatusEx
	stat.Length = uint32(unsafe.Sizeof(stat))
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	globalMemoryStatusEx := kernel32.NewProc("GlobalMemoryStatusEx")
	r, _, callErr := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&stat)))
	if r != 0 {
		return int64(stat.TotalPhys / (1024 * 1024)), int64(stat.AvailPhys / (1024 * 1024)), true
	}
	// GlobalMemoryStatusEx failing is rare enough that the win32 error is
	// worth a log line rather than silently returning the same 0/0 a
	// genuinely-zero reading would - the caller previously had no way to
	// distinguish "call failed" from "host reports 0 MB" at all.
	log.Printf("admin: GlobalMemoryStatusEx failed: %v", callErr)
	return 0, 0, false
}
