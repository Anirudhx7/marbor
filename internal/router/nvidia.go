package router

import (
	"context"
	"encoding/xml"
	"os/exec"
	"strconv"
	"strings"
	"time"
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
	MinorNumber      string      `xml:"minor_number"`
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

func parseMiB(s string) (int64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "MiB"))
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v, err == nil
}

func parseCelsius(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "C"))
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}

func parseWatts(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "W"))
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}

// queryGPU returns GPU stats from nvidia-smi for the given GPU index.
// Returns zero-value GPUStats and false if nvidia-smi is unavailable or fails.
func queryGPU(gpuIndex int) (GPUStats, bool) {
	statsMap, ok := queryAllGPUs()
	if !ok {
		return GPUStats{}, false
	}
	stats, found := statsMap[gpuIndex]
	return stats, found
}

func parseNvidiaSMIXML(data []byte, gpuIndex int) (GPUStats, bool) {
	statsMap, ok := parseAllNvidiaSMIXML(data)
	if !ok {
		return GPUStats{}, false
	}
	stats, found := statsMap[gpuIndex]
	return stats, found
}

// queryAllGPUs returns stats for all GPUs on the host by executing nvidia-smi once.
func queryAllGPUs() (map[int]GPUStats, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "-q", "-x").Output()
	if err != nil {
		return nil, false
	}
	return parseAllNvidiaSMIXML(out)
}

func parseAllNvidiaSMIXML(data []byte) (map[int]GPUStats, bool) {
	var log nvidiaSMILog
	if err := xml.Unmarshal(data, &log); err != nil {
		return nil, false
	}
	statsMap := make(map[int]GPUStats)
	for i, gpu := range log.GPUs {
		total, totalOK := parseMiB(gpu.FBMemory.Total)
		used, usedOK := parseMiB(gpu.FBMemory.Used)
		temp, tempOK := parseCelsius(gpu.Temperature.GPUTemp)
		if !totalOK || !usedOK || !tempOK {
			continue
		}
		stats := GPUStats{
			VRAMTotalMB: total,
			VRAMUsedMB:  used,
			TempCelsius: temp,
		}
		// Power draw may be legitimately absent ("" or "N/A") on some cards -
		// only a present-but-unparseable value is treated as a parse failure.
		raw := gpu.PowerReadings.PowerDraw
		if raw == "" || raw == "N/A" {
			raw = gpu.GPUPowerReadings.PowerDraw
		}
		if raw != "" && raw != "N/A" {
			watts, powerOK := parseWatts(raw)
			if !powerOK {
				continue
			}
			stats.PowerDrawW = watts
		}
		// Key by the GPU's own minor_number when present and parseable
		// (nvidia-smi -q -x's stable per-GPU identifier - the number
		// operators reference via CUDA_VISIBLE_DEVICES/nvidia-smi -i),
		// instead of trusting document order to match the
		// operator-declared NvidiaIndex. Falls back to loop position
		// only if minor_number is missing/unparseable, preserving the
		// old behavior rather than dropping the GPU entirely.
		key := i
		if mn, err := strconv.Atoi(strings.TrimSpace(gpu.MinorNumber)); err == nil {
			key = mn
		}
		statsMap[key] = stats
	}
	return statsMap, true
}
