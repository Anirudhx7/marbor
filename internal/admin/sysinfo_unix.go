//go:build !windows

package admin

import (
	"os"
	"strconv"
	"strings"
)

func readSystemMemory() (totalMB, freeMB int64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvailable int64
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			key := strings.TrimSuffix(parts[0], ":")
			val, _ := strconv.ParseInt(parts[1], 10, 64)
			if key == "MemTotal" {
				memTotal = val / 1024 // kB to MB
			} else if key == "MemAvailable" {
				memAvailable = val / 1024 // kB to MB
			}
		}
	}
	if memTotal > 0 {
		if memAvailable == 0 {
			memAvailable = memTotal
		}
		return memTotal, memAvailable
	}
	return 0, 0
}
