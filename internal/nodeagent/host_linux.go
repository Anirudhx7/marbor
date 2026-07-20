//go:build linux

package nodeagent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CPU percent needs two /proc/stat samples to compute a delta, so the first
// call after process start (or after a long gap) reports no CPU figure
// (unknown, not fabricated - R1) and every call after that reports the
// utilization since the previous call. This deliberately avoids sleeping
// inside a request handler (which would slow every /v1/status poll) -
// mirrors the mesh's own poll-cycle-driven nvidia-smi cadence.
var (
	cpuMu     sync.Mutex
	prevTotal uint64
	prevIdle  uint64
	havePrev  bool
)

func collectHost() *HostTelemetry {
	h := &HostTelemetry{}
	if cpu, ok := readCPUPercent(); ok {
		h.CPUPercent = &cpu
	}
	if used, total, ok := readRAMStatsMB(); ok {
		h.RAMUsedMB = used
		h.RAMTotalMB = total
	}
	if free, total, ok := readDiskStatsGB("/"); ok {
		h.DiskFreeGB = free
		h.DiskTotalGB = total
	}
	if name, err := os.Hostname(); err == nil {
		h.Hostname = name
	}
	if uptime, boot, ok := readUptimeAndBoot(); ok {
		h.UptimeSeconds = uptime
		h.BootTime = boot
	}
	return h
}

func readCPUPercent() (float64, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, false
	}
	fields := strings.Fields(sc.Text())
	// "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, false
	}
	var total, idle uint64
	for i, tok := range fields[1:] {
		v, err := strconv.ParseUint(tok, 10, 64)
		if err != nil {
			continue
		}
		total += v
		if i == 3 { // idle is the 4th value (index 3) after "cpu"
			idle = v
		}
	}

	cpuMu.Lock()
	defer cpuMu.Unlock()
	if !havePrev {
		prevTotal, prevIdle, havePrev = total, idle, true
		return 0, false
	}
	deltaTotal := total - prevTotal
	deltaIdle := idle - prevIdle
	prevTotal, prevIdle = total, idle
	if deltaTotal == 0 {
		return 0, false
	}
	pct := (1.0 - float64(deltaIdle)/float64(deltaTotal)) * 100.0
	if pct < 0 {
		pct = 0
	}
	return pct, true
}

// readRAMStatsMB returns (used, total) in MB from /proc/meminfo.
func readRAMStatsMB() (int64, int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var totalKB, availKB int64
	haveTotal, haveAvail := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseMeminfoLine(line)
			haveTotal = true
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = parseMeminfoLine(line)
			haveAvail = true
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, false
	}
	usedKB := totalKB - availKB
	if usedKB < 0 {
		usedKB = 0
	}
	return usedKB / 1024, totalKB / 1024, true
}

func parseMeminfoLine(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(fields[1], 10, 64)
	return v
}

// readDiskStatsGB returns (free, total) in GB for path via syscall.Statfs.
func readDiskStatsGB(path string) (float64, float64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	freeBytes := uint64(stat.Bavail) * uint64(stat.Bsize)
	totalBytes := uint64(stat.Blocks) * uint64(stat.Bsize)
	return float64(freeBytes) / 1e9, float64(totalBytes) / 1e9, true
}

// readUptimeAndBoot reads /proc/uptime for the seconds-since-boot figure and
// derives boot_time as now - uptime. A read/parse failure omits both fields
// (unknown, never fabricated - R1) rather than reporting a guessed uptime.
func readUptimeAndBoot() (int64, int64, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, 0, false
	}
	uptimeSec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, false
	}
	uptime := int64(uptimeSec)
	boot := time.Now().Unix() - uptime
	return uptime, boot, true
}
