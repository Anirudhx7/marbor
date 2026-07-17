package nodeagent

import (
	"context"
	"encoding/xml"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU stats are collected by shelling out to nvidia-smi, using the same
// query flags and units as internal/router/nvidia.go's local-node polling
// path (-q -x, MiB, Celsius, Watts) so the agent and the mesh's own local
// nvidia-smi reader never disagree on how a number was derived. The two
// packages don't share the exact function (router's queryAllGPUs is
// unexported, and this package must not import router to avoid coupling the
// mesh binary's poller internals to the standalone agent binary), so the
// parsing here is a deliberate faithful copy, extended with fan_speed since
// the mesh's own local-GPU path doesn't currently report fan.
type nvidiaSMILog struct {
	GPUs []nvidiaGPU `xml:"gpu"`
}

type nvidiaGPU struct {
	FanSpeed         string      `xml:"fan_speed"`
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

// collectGPU returns telemetry for GPU index 0 (the first GPU nvidia-smi
// reports), or (zero, false) if nvidia-smi is unavailable, fails, or the
// host has no GPU. The wire schema carries a single "gpu" object (not an
// array), matching the node-agent's target topology of one agent process
// per node reporting on its primary accelerator.
func collectGPU() (GPUTelemetry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "-q", "-x").Output()
	if err != nil {
		return GPUTelemetry{}, false
	}
	return parseNvidiaSMIXML(out)
}

func parseNvidiaSMIXML(data []byte) (GPUTelemetry, bool) {
	var log nvidiaSMILog
	if err := xml.Unmarshal(data, &log); err != nil || len(log.GPUs) == 0 {
		return GPUTelemetry{}, false
	}
	gpu := log.GPUs[0]

	var out GPUTelemetry
	out.VRAMTotalMB = parseMiB(gpu.FBMemory.Total)
	out.VRAMUsedMB = parseMiB(gpu.FBMemory.Used)

	temp := parseCelsius(gpu.Temperature.GPUTemp)
	out.TemperatureC = &temp

	var watts float64
	if gpu.PowerReadings.PowerDraw != "" && gpu.PowerReadings.PowerDraw != "N/A" {
		watts = parseWatts(gpu.PowerReadings.PowerDraw)
	} else {
		watts = parseWatts(gpu.GPUPowerReadings.PowerDraw)
	}
	out.PowerWatts = &watts

	if fan, ok := parsePercent(gpu.FanSpeed); ok {
		out.FanPercent = &fan
	}
	return out, true
}
