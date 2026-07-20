package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestIntelCollectorAvailableWhenOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/bin/xpu-smi", nil })
	if !(intelCollector{}).Available(context.Background()) {
		t.Error("expected Available()=true when xpu-smi resolves on PATH")
	}
}

func TestIntelCollectorUnavailableWhenNotOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if (intelCollector{}).Available(context.Background()) {
		t.Error("expected Available()=false when xpu-smi is not on PATH")
	}
}

// sampleXPUDiscoveryJSON / sampleXPUStatsJSON are representative samples
// built from xpu-smi's publicly documented `discovery -j` / `stats -j`
// output shapes - not captured from real hardware (see gpu_intel.go's file
// header).
const sampleXPUDiscoveryJSON = `{
	"device_list": [
		{"device_id": 0, "device_name": "Intel(R) Data Center GPU Max 1550"}
	]
}`

const sampleXPUStatsJSON = `{
	"device_level": [
		{"metrics_type": "GPU_TEMPERATURE", "value": "62"},
		{"metrics_type": "GPU_POWER", "value": "210.5"},
		{"metrics_type": "GPU_UTILIZATION", "value": "45"},
		{"metrics_type": "GPU_MEMORY_TOTAL_BYTES", "value": "68719476736"},
		{"metrics_type": "GPU_MEMORY_USED_BYTES", "value": "17179869184"}
	]
}`

func TestApplyXPUStats(t *testing.T) {
	info := GPUInfo{Index: 0, Vendor: "intel", Model: "Intel(R) Data Center GPU Max 1550"}
	applyXPUStats(&info, []byte(sampleXPUStatsJSON))

	if info.TemperatureC == nil || *info.TemperatureC != 62 {
		t.Errorf("TemperatureC = %v, want 62", info.TemperatureC)
	}
	if info.PowerWatts == nil || *info.PowerWatts != 210.5 {
		t.Errorf("PowerWatts = %v, want 210.5", info.PowerWatts)
	}
	if info.CorePercent == nil || *info.CorePercent != 45 {
		t.Errorf("CorePercent = %v, want 45", info.CorePercent)
	}
	if info.VRAMTotalMB != 65536 || info.VRAMUsedMB != 16384 {
		t.Errorf("VRAM = %d/%d, want 16384/65536", info.VRAMUsedMB, info.VRAMTotalMB)
	}
	if info.FanPercent != nil {
		t.Errorf("FanPercent should never be set for Intel Data Center GPUs, got %v", *info.FanPercent)
	}
}

func TestApplyXPUStatsMalformedLeavesFieldsUnset(t *testing.T) {
	info := GPUInfo{Index: 0, Vendor: "intel"}
	applyXPUStats(&info, []byte(`not json at all`))
	if info.TemperatureC != nil || info.PowerWatts != nil || info.CorePercent != nil {
		t.Error("expected every reading to stay nil when stats JSON is malformed, not fabricated")
	}
}

func TestDetectGPUCollectorPicksIntelWhenOthersAbsent(t *testing.T) {
	old := gpuCandidates
	defer func() { gpuCandidates = old }()
	gpuCandidates = []GPUCollector{nvidiaCollector{}, rocmCollector{}, intelCollector{}}

	withLookPath(t, func(name string) (string, error) {
		if name == "xpu-smi" {
			return "/usr/bin/xpu-smi", nil
		}
		return "", errors.New("not found")
	})
	c := detectGPUCollector(context.Background())
	if c.Name() != "intel" {
		t.Errorf("Name() = %q, want intel", c.Name())
	}
}

// Ensures the discovery-JSON shape itself decodes as documented - the full
// Collect() path also shells out to `xpu-smi stats`, which isn't exercised
// here (no real binary in this environment); TestApplyXPUStats above covers
// the stats-parsing half directly.
func TestXPUDiscoveryResponseDecodes(t *testing.T) {
	var disc xpuDiscoveryResponse
	if err := json.Unmarshal([]byte(sampleXPUDiscoveryJSON), &disc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(disc.DeviceList) != 1 {
		t.Fatalf("len(DeviceList) = %d, want 1", len(disc.DeviceList))
	}
	if disc.DeviceList[0].DeviceName != "Intel(R) Data Center GPU Max 1550" {
		t.Errorf("DeviceName = %q, want Intel(R) Data Center GPU Max 1550", disc.DeviceList[0].DeviceName)
	}
}
