package marboragent

import (
	"context"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// lookPath is exec.LookPath indirected through a package variable so tests
// can simulate "nvidia-smi present/absent" deterministically without
// depending on whether the actual CI/sandbox host has it on PATH.
var lookPath = exec.LookPath

// nvidiaCollector is the GPUCollector implementation for NVIDIA GPUs via
// nvidia-smi. First (and, in v1, only) implementation of GPUCollector - see
// gpu.go for the interface and how future vendors (ROCm, Apple Metal, Intel)
// plug in alongside it. One agent process reports every NVIDIA GPU on the
// host as a single GPUBlock - never one agent per GPU (see
// .local/specs/telemetry-v1-spec.md section 6's decision record).
type nvidiaCollector struct{}

func (nvidiaCollector) Name() string { return "nvidia" }

// Available reports whether nvidia-smi resolves on PATH. Cheap path lookup,
// not a full probe - matches how router.go's runtime auto-detect treats
// backend presence as a one-time startup check, not a per-cycle cost.
func (nvidiaCollector) Available(ctx context.Context) bool {
	_, err := lookPath("nvidia-smi")
	return err == nil
}

func (nvidiaCollector) Collect(ctx context.Context) (GPUBlock, error) {
	out, err := exec.CommandContext(ctx, "nvidia-smi", "-q", "-x").Output()
	if err != nil {
		return GPUBlock{}, fmt.Errorf("nvidia-smi: %w", err)
	}
	block, ok := parseNvidiaSMIXML(out)
	if !ok {
		return GPUBlock{}, fmt.Errorf("nvidia-smi: could not parse output")
	}
	return block, nil
}

// Same query flags and units internal/router/nvidia.go's local-node polling
// path uses (-q -x, MiB, Celsius, Watts), so the agent and the mesh's own
// local nvidia-smi reader never disagree on how a number was derived. The
// two packages don't share the exact function (router's queryAllGPUs is
// unexported, and this package must not import router to avoid coupling the
// mesh binary's poller internals to the standalone agent binary), so the
// parsing here is a deliberate faithful copy, extended with fan_speed,
// utilization (core%), and driver/CUDA version since the mesh's own
// local-GPU path doesn't currently report those.
type nvidiaSMILog struct {
	DriverVersion string      `xml:"driver_version"`
	CUDAVersion   string      `xml:"cuda_version"`
	GPUs          []nvidiaGPU `xml:"gpu"`
}

type nvidiaGPU struct {
	ProductName      string      `xml:"product_name"`
	FanSpeed         string      `xml:"fan_speed"`
	FBMemory         nvidiaMem   `xml:"fb_memory_usage"`
	Temperature      nvidiaTemp  `xml:"temperature"`
	PowerReadings    nvidiaPower `xml:"power_readings"`
	GPUPowerReadings nvidiaPower `xml:"gpu_power_readings"`
	Utilization      nvidiaUtil  `xml:"utilization"`
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

type nvidiaUtil struct {
	GPUUtil string `xml:"gpu_util"`
}

func parseMiB(s string) int64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "MiB"))
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// parseCelsius parses a temperature reading like "67 C". Returns ok=false
// for "N/A" or anything unparseable (a real gap on some cards/drivers where
// the sensor isn't reported) - callers must omit the field rather than use
// the zero value as a real 0°C reading (R1).
func parseCelsius(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, false
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "C"))
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseWatts parses a power reading like "218.00 W". Returns ok=false for
// "N/A" or anything unparseable, same R1 reasoning as parseCelsius.
func parseWatts(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, false
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "W"))
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parsePercent(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, false
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseNvidiaSMIXML parses `nvidia-smi -q -x` output into a GPUBlock
// carrying every reported <gpu> element (multi-GPU array, per
// telemetry-v1-spec.md section 6 - one agent process owns the whole node,
// including every GPU on it) plus the host-level driver/CUDA version.
func parseNvidiaSMIXML(data []byte) (GPUBlock, bool) {
	var log nvidiaSMILog
	if err := xml.Unmarshal(data, &log); err != nil || len(log.GPUs) == 0 {
		return GPUBlock{}, false
	}

	block := GPUBlock{
		Count:         len(log.GPUs),
		Vendor:        "nvidia",
		DriverVersion: strings.TrimSpace(log.DriverVersion),
		CUDAVersion:   strings.TrimSpace(log.CUDAVersion),
		Devices:       make([]GPUInfo, 0, len(log.GPUs)),
	}

	for i, gpu := range log.GPUs {
		info := GPUInfo{
			Index:       i,
			Vendor:      "nvidia",
			Model:       strings.TrimSpace(gpu.ProductName),
			VRAMTotalMB: parseMiB(gpu.FBMemory.Total),
			VRAMUsedMB:  parseMiB(gpu.FBMemory.Used),
		}

		if temp, ok := parseCelsius(gpu.Temperature.GPUTemp); ok {
			info.TemperatureC = &temp
		}

		// Power reading falls back from power_readings to gpu_power_readings
		// when the primary is unavailable ("N/A") - only set PowerWatts if
		// whichever source is actually used parses successfully; never
		// fabricate a fallback-of-a-fallback 0 (R1).
		watts, ok := parseWatts(gpu.PowerReadings.PowerDraw)
		if !ok {
			watts, ok = parseWatts(gpu.GPUPowerReadings.PowerDraw)
		}
		if ok {
			info.PowerWatts = &watts
		}

		if fan, ok := parsePercent(gpu.FanSpeed); ok {
			info.FanPercent = &fan
		}
		if core, ok := parsePercent(gpu.Utilization.GPUUtil); ok {
			info.CorePercent = &core
		}

		block.Devices = append(block.Devices, info)
	}

	return block, true
}
