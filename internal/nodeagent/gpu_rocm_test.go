package nodeagent

import (
	"context"
	"errors"
	"testing"
)

func TestRocmCollectorAvailableWhenOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/bin/rocm-smi", nil })
	if !(rocmCollector{}).Available(context.Background()) {
		t.Error("expected Available()=true when rocm-smi resolves on PATH")
	}
}

func TestRocmCollectorUnavailableWhenNotOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if (rocmCollector{}).Available(context.Background()) {
		t.Error("expected Available()=false when rocm-smi is not on PATH")
	}
}

// sampleROCmSMIJSON is a representative sample of `rocm-smi -a --json`
// output built from ROCm's publicly documented key names - not captured
// from real hardware (see gpu_rocm.go's file header).
const sampleROCmSMIJSON = `{
	"card0": {
		"Card series": "AMD Instinct MI210",
		"Temperature (Sensor edge) (C)": "58.0",
		"Fan speed (%)": "38",
		"Average Graphics Package Power (W)": "180.0",
		"GPU use (%)": "72",
		"VRAM Total Memory (B)": "68719476736",
		"VRAM Total Used Memory (B)": "34359738368"
	},
	"card1": {
		"Card series": "AMD Instinct MI210",
		"Temperature (Sensor edge) (C)": "N/A",
		"Fan speed (%)": "22",
		"GPU use (%)": "5",
		"VRAM Total Memory (B)": "68719476736",
		"VRAM Total Used Memory (B)": "1073741824"
	}
}`

func TestParseROCmSMIJSONMultiGPU(t *testing.T) {
	block, ok := parseROCmSMIJSON([]byte(sampleROCmSMIJSON))
	if !ok {
		t.Fatal("expected ok=true for valid JSON")
	}
	if block.Count != 2 {
		t.Fatalf("Count = %d, want 2", block.Count)
	}
	if block.Vendor != "rocm" {
		t.Errorf("Vendor = %q, want rocm", block.Vendor)
	}
	if len(block.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2", len(block.Devices))
	}

	d0 := block.Devices[0]
	if d0.Index != 0 {
		t.Errorf("Devices[0].Index = %d, want 0", d0.Index)
	}
	if d0.Model != "AMD Instinct MI210" {
		t.Errorf("Devices[0].Model = %q, want AMD Instinct MI210", d0.Model)
	}
	if d0.TemperatureC == nil || *d0.TemperatureC != 58 {
		t.Errorf("Devices[0].TemperatureC = %v, want 58", d0.TemperatureC)
	}
	if d0.FanPercent == nil || *d0.FanPercent != 38 {
		t.Errorf("Devices[0].FanPercent = %v, want 38", d0.FanPercent)
	}
	if d0.PowerWatts == nil || *d0.PowerWatts != 180 {
		t.Errorf("Devices[0].PowerWatts = %v, want 180", d0.PowerWatts)
	}
	if d0.CorePercent == nil || *d0.CorePercent != 72 {
		t.Errorf("Devices[0].CorePercent = %v, want 72", d0.CorePercent)
	}
	if d0.VRAMTotalMB != 65536 || d0.VRAMUsedMB != 32768 {
		t.Errorf("Devices[0] VRAM = %d/%d, want 32768/65536", d0.VRAMUsedMB, d0.VRAMTotalMB)
	}

	// card1 reports "N/A" for temperature and omits power entirely - both
	// must stay nil rather than a fabricated 0 (R1).
	d1 := block.Devices[1]
	if d1.TemperatureC != nil {
		t.Errorf("Devices[1].TemperatureC should be nil for N/A, got %v", *d1.TemperatureC)
	}
	if d1.PowerWatts != nil {
		t.Errorf("Devices[1].PowerWatts should be nil when absent, got %v", *d1.PowerWatts)
	}
}

func TestParseROCmSMIJSONNoCards(t *testing.T) {
	_, ok := parseROCmSMIJSON([]byte(`{}`))
	if ok {
		t.Error("expected ok=false when no cardN keys are present")
	}
}

func TestParseROCmSMIJSONMalformed(t *testing.T) {
	_, ok := parseROCmSMIJSON([]byte(`not json at all`))
	if ok {
		t.Error("expected ok=false for malformed JSON")
	}
}

func TestDetectGPUCollectorPicksRocmWhenNvidiaAbsent(t *testing.T) {
	old := gpuCandidates
	defer func() { gpuCandidates = old }()
	gpuCandidates = []GPUCollector{nvidiaCollector{}, rocmCollector{}}

	withLookPath(t, func(name string) (string, error) {
		if name == "rocm-smi" {
			return "/usr/bin/rocm-smi", nil
		}
		return "", errors.New("not found")
	})
	c := detectGPUCollector(context.Background())
	if c.Name() != "rocm" {
		t.Errorf("Name() = %q, want rocm", c.Name())
	}
}
