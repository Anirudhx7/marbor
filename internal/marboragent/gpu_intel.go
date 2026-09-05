package marboragent

// Intel GPUCollector, via `xpu-smi` (Intel's Data Center GPU management CLI -
// the closest Intel equivalent to nvidia-smi/rocm-smi in design: a one-shot
// scriptable query tool, unlike intel_gpu_top which is built for a live
// interactive/streaming view and is a worse fit for one-shot collection).
// UNVERIFIED ON REAL HARDWARE (no Intel GPU available in this environment,
// 2026-07-20): built against xpu-smi's publicly
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
	"time"
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

		// Give each remaining device a fair share of whatever's left of the
		// caller's overall budget (refresh()'s single 5s ctx), rather than
		// letting devices earlier in discovery order silently exhaust the
		// shared deadline and leave every later device with none at all -
		// bounds degradation on a large multi-GPU node to even partial
		// readings across devices instead of full readings on the first few
		// and none on the rest.
		statsCtx, cancel := perDeviceContext(ctx, len(disc.DeviceList)-i)
		statsOut, err := exec.CommandContext(statsCtx, "xpu-smi", "stats", "-d", strconv.Itoa(dev.DeviceID), "-j").Output()
		cancel()
		if err == nil {
			applyXPUStats(&info, statsOut)
		}
		// A failed per-device stats call still yields a device entry with
		// its Model (from discovery) and every reading omitted - not
		// dropped and not zero-filled.

		block.Devices = append(block.Devices, info)
	}
	return block, nil
}

// perDeviceContext derives a sub-context bounded by an equal fraction
// (remaining / devicesLeft) of parent's remaining deadline. If parent has no
// deadline (e.g. a test calling Collect directly with context.Background()),
// it's returned unmodified with a no-op cancel.
func perDeviceContext(parent context.Context, devicesLeft int) (context.Context, context.CancelFunc) {
	deadline, ok := parent.Deadline()
	if !ok || devicesLeft <= 0 {
		return parent, func() {}
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithTimeout(parent, 0)
	}
	return context.WithTimeout(parent, remaining/time.Duration(devicesLeft))
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
	// is intentionally never set here, not an oversight (no such
	// measurement exists to report, same as nvidia-smi's own "N/A" case).
}
