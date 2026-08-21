package marboragent

// Intel GPUCollector, via `xpu-smi` (Intel's Data Center GPU management CLI -
// the closest Intel equivalent to nvidia-smi/rocm-smi in design: a one-shot
// scriptable query tool, unlike intel_gpu_top which is built for a live
// interactive/streaming view and is a worse fit for one-shot collection).
// UNVERIFIED ON REAL HARDWARE (Anirudh's explicit call, 2026-07-20 - no
// Intel GPU available in this environment): built against xpu-smi's publicly
// documented `discovery -j` / `stats -d <id> -j` output shapes. Treat this as
// needing a real-hardware validation pass before being fully trusted, same
// caveat as gpu_rocm.go.
//
// xpu-smi's design requires two calls where nvidia-smi/rocm-smi need only
// one: `discovery -j` enumerates device IDs (and, for Data Center Max/Flex
// cards, device model/name), then `stats -d <id> -j` is a per-device query -
// there is no single "all devices, all stats" invocation in the documented
// CLI. Collect therefore issues one discovery call plus one stats call per
// discovered device, same per-cycle cost shape as any multi-device host, and
// still returns everything in a single GPUBlock like every other collector.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type intelCollector struct{}

func (intelCollector) Name() string { return "intel" }

func (intelCollector) Available(ctx context.Context) bool {
	_, err := lookPath("xpu-smi")
	return err == nil
}

// xpuDiscoveryResponse mirrors `xpu-smi discovery -j`'s documented shape:
// a top-level "device_list" array, each entry identifying one device.
type xpuDiscoveryResponse struct {
	DeviceList []xpuDiscoveryDevice `json:"device_list"`
}

type xpuDiscoveryDevice struct {
	DeviceID   int    `json:"device_id"`
	DeviceName string `json:"device_name"`
}

// xpuStatsResponse mirrors `xpu-smi stats -d <id> -j`'s documented shape: a
// flat "device_level" array of {metrics_type, value} pairs - deliberately
// decoded permissively (metricsType/value both plain strings) since the
// exact metrics_type enum names are the least stable part of the documented
// contract across xpu-smi releases.
type xpuStatsResponse struct {
	DeviceLevel []xpuMetric `json:"device_level"`
}

type xpuMetric struct {
	MetricsType string `json:"metrics_type"`
	Value       string `json:"value"`
}

func (intelCollector) Collect(ctx context.Context) (GPUBlock, error) {
	discOut, err := exec.CommandContext(ctx, "xpu-smi", "discovery", "-j").Output()
	if err != nil {
		return GPUBlock{}, fmt.Errorf("xpu-smi discovery: %w", err)
	}
	var disc xpuDiscoveryResponse
	if err := json.Unmarshal(discOut, &disc); err != nil || len(disc.DeviceList) == 0 {
		return GPUBlock{}, fmt.Errorf("xpu-smi discovery: could not parse output")
	}

	block := GPUBlock{Count: len(disc.DeviceList), Vendor: "intel", Devices: make([]GPUInfo, 0, len(disc.DeviceList))}
	for i, dev := range disc.DeviceList {
		info := GPUInfo{Index: i, Vendor: "intel", Model: strings.TrimSpace(dev.DeviceName)}

		statsOut, err := exec.CommandContext(ctx, "xpu-smi", "stats", "-d", strconv.Itoa(dev.DeviceID), "-j").Output()
		if err == nil {
			applyXPUStats(&info, statsOut)
		}
		// A failed per-device stats call still yields a device entry with
		// its Model (from discovery) and every reading omitted - not
		// dropped and not zero-filled (R1).

		block.Devices = append(block.Devices, info)
	}
	return block, nil
}

// xpuField tries every known metrics_type spelling in turn - same reasoning
// as gpu_rocm.go's rocmField: the documented enum names have already shifted
// between xpu-smi releases in practice.
func xpuField(metrics []xpuMetric, names ...string) (string, bool) {
	for _, m := range metrics {
		for _, n := range names {
			if m.MetricsType == n && strings.TrimSpace(m.Value) != "" {
				return m.Value, true
			}
		}
	}
	return "", false
}

func applyXPUStats(info *GPUInfo, statsOut []byte) {
	var stats xpuStatsResponse
	if err := json.Unmarshal(statsOut, &stats); err != nil {
		return
	}
	if temp, ok := xpuField(stats.DeviceLevel, "GPU_TEMPERATURE", "temperature"); ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(temp), 64); err == nil {
			info.TemperatureC = &v
		}
	}
	if power, ok := xpuField(stats.DeviceLevel, "GPU_POWER", "power"); ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(power), 64); err == nil {
			info.PowerWatts = &v
		}
	}
	if util, ok := xpuField(stats.DeviceLevel, "GPU_UTILIZATION", "gpu_utilization"); ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(util), 64); err == nil {
			info.CorePercent = &v
		}
	}
	if total, ok := xpuField(stats.DeviceLevel, "GPU_MEMORY_TOTAL_BYTES", "memory_total"); ok {
		if v, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64); err == nil {
			info.VRAMTotalMB = v / (1024 * 1024)
		}
	}
	if used, ok := xpuField(stats.DeviceLevel, "GPU_MEMORY_USED_BYTES", "memory_used"); ok {
		if v, err := strconv.ParseInt(strings.TrimSpace(used), 10, 64); err == nil {
			info.VRAMUsedMB = v / (1024 * 1024)
		}
	}
	// Intel Data Center GPUs are typically fanless (liquid/chassis-cooled),
	// and xpu-smi does not document a fan-speed metric at all - FanPercent
	// is intentionally never set here, not an oversight (R1: no such
	// measurement exists to report, same as nvidia-smi's own "N/A" case).
}
