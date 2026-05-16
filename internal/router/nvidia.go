package router

import (
	"encoding/xml"
	"os/exec"
	"strconv"
	"strings"
)

type GPUStats struct {
	VRAMTotalMB int64
	VRAMUsedMB  int64
	TempCelsius float64
	PowerDrawW  float64
}

type nvidiaSMILog struct {
	GPUs []nvidiaGPU `xml:"gpu"`
}

type nvidiaGPU struct {
	FBMemory         nvidiaMem   `xml:"fb_memory_usage"`
	Temperature      nvidiaTemp  `xml:"temperature"`
	PowerReadings    nvidiaPower `xml:"power_readings"`
	GPUPowerReadings nvidiaPower `xml:"gpu_power_readings"`
}

type nvidiaMem struct {
	Total string `xml:"total"`
	Used  string `xml:"used"`
}

type nvidiaTemp struct {
	GPUTemp string `xml:"gpu_temp"`
}

type nvidiaPower struct {
	PowerDraw string `xml:"power_draw"`
}

func parseMiB(s string) int64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "MiB"))
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

func parseCelsius(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "C"))
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func parseWatts(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "W"))
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// queryGPU returns GPU stats from nvidia-smi for the given GPU index.
// Returns zero-value GPUStats and false if nvidia-smi is unavailable or fails.
func queryGPU(gpuIndex int) (GPUStats, bool) {
	out, err := exec.Command("nvidia-smi", "-q", "-x").Output()
	if err != nil {
		return GPUStats{}, false
	}
	return parseNvidiaSMIXML(out, gpuIndex)
}

func parseNvidiaSMIXML(data []byte, gpuIndex int) (GPUStats, bool) {
	var log nvidiaSMILog
	if err := xml.Unmarshal(data, &log); err != nil {
		return GPUStats{}, false
	}
	if gpuIndex >= len(log.GPUs) {
		return GPUStats{}, false
	}
	gpu := log.GPUs[gpuIndex]
	stats := GPUStats{
		VRAMTotalMB: parseMiB(gpu.FBMemory.Total),
		VRAMUsedMB:  parseMiB(gpu.FBMemory.Used),
		TempCelsius: parseCelsius(gpu.Temperature.GPUTemp),
	}
	// Driver version determines which tag is used for power readings.
	if gpu.PowerReadings.PowerDraw != "" && gpu.PowerReadings.PowerDraw != "N/A" {
		stats.PowerDrawW = parseWatts(gpu.PowerReadings.PowerDraw)
	} else {
		stats.PowerDrawW = parseWatts(gpu.GPUPowerReadings.PowerDraw)
	}
	return stats, true
}
