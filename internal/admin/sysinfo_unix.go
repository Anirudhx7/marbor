//go:build !windows

package admin

import (
	"os"
	"strconv"
	"strings"
)

func readSystemMemory() (totalMB, freeMB int64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var memTotal, memFree int64
	var memAvailable int64
	haveMemAvailable := false
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			key := strings.TrimSuffix(parts[0], ":")
			val, _ := strconv.ParseInt(parts[1], 10, 64)
			switch key {
			case "MemTotal":
				memTotal = val / 1024 // kB to MB
			case "MemFree":
				memFree = val / 1024 // kB to MB
			case "MemAvailable":
				memAvailable = val / 1024 // kB to MB
				haveMemAvailable = true
			}
		}
	}
	if memTotal > 0 {
		// MemAvailable==0 is genuinely ambiguous between "kernel doesn't
		// report it" and "host is memory-exhausted" - substituting memTotal
		// in either case fabricated "all RAM free" exactly when memory could
		// be actually exhausted. MemFree is always reported by the
		// kernel, so use it whenever MemAvailable itself is absent from the
		// file; never substitute memTotal for a genuine 0 value.
		if !haveMemAvailable {
			return memTotal, memFree, true
		}
		return memTotal, memAvailable, true
	}
	// memTotal<=0 means the file parsed but didn't contain a usable
	// MemTotal line - no distinguishable "0 MB machine" case exists in
	// practice, so this is "couldn't read," not "read a real zero."
	return 0, 0, false
}
