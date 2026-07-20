package nodeagent

import (
	"context"
	"errors"
	"testing"
)

func TestAppleCollectorAvailableWhenOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/sbin/system_profiler", nil })
	if !(appleCollector{}).Available(context.Background()) {
		t.Error("expected Available()=true when system_profiler resolves on PATH")
	}
}

func TestAppleCollectorUnavailableWhenNotOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if (appleCollector{}).Available(context.Background()) {
		t.Error("expected Available()=false when system_profiler is not on PATH")
	}
}

// sampleSPDisplaysJSON is a representative sample of
// `system_profiler SPDisplaysDataType -json` built from Apple's publicly
// documented output shape - not captured from real hardware (see
// gpu_apple.go's file header).
const sampleSPDisplaysJSON = `{
	"SPDisplaysDataType": [
		{
			"_name": "Apple M3 Max",
			"sppci_cores": "40",
			"spdisplays_mtlgpufamilysupport": "spdisplays_metal3"
		}
	]
}`

func TestParseSPDisplaysJSON(t *testing.T) {
	block, ok := parseSPDisplaysJSON([]byte(sampleSPDisplaysJSON))
	if !ok {
		t.Fatal("expected ok=true for valid JSON")
	}
	if block.Count != 1 {
		t.Fatalf("Count = %d, want 1", block.Count)
	}
	if block.Vendor != "apple" {
		t.Errorf("Vendor = %q, want apple", block.Vendor)
	}
	if len(block.Devices) != 1 {
		t.Fatalf("len(Devices) = %d, want 1", len(block.Devices))
	}

	d0 := block.Devices[0]
	if d0.Model != "Apple M3 Max" {
		t.Errorf("Devices[0].Model = %q, want Apple M3 Max", d0.Model)
	}
	// No temperature/fan/power/VRAM is ever fabricated for this collector -
	// system_profiler exposes none of it unprivileged (R1).
	if d0.TemperatureC != nil || d0.FanPercent != nil || d0.PowerWatts != nil {
		t.Error("expected TemperatureC/FanPercent/PowerWatts to stay nil - system_profiler reports none of them")
	}
	if d0.VRAMTotalMB != 0 || d0.VRAMUsedMB != 0 {
		t.Error("expected VRAM fields to stay zero/unset - unified memory has no separate VRAM figure to report")
	}
}

func TestParseSPDisplaysJSONNoEntries(t *testing.T) {
	_, ok := parseSPDisplaysJSON([]byte(`{"SPDisplaysDataType": []}`))
	if ok {
		t.Error("expected ok=false when SPDisplaysDataType is empty")
	}
}

func TestParseSPDisplaysJSONMalformed(t *testing.T) {
	_, ok := parseSPDisplaysJSON([]byte(`not json at all`))
	if ok {
		t.Error("expected ok=false for malformed JSON")
	}
}

func TestDetectGPUCollectorPicksAppleWhenOthersAbsent(t *testing.T) {
	old := gpuCandidates
	defer func() { gpuCandidates = old }()
	gpuCandidates = []GPUCollector{nvidiaCollector{}, rocmCollector{}, intelCollector{}, appleCollector{}}

	withLookPath(t, func(name string) (string, error) {
		if name == "system_profiler" {
			return "/usr/sbin/system_profiler", nil
		}
		return "", errors.New("not found")
	})
	c := detectGPUCollector(context.Background())
	if c.Name() != "apple" {
		t.Errorf("Name() = %q, want apple", c.Name())
	}
}
