package nodeagent

import (
	"context"
	"errors"
	"testing"
)

// withLookPath temporarily replaces the package-level lookPath seam so
// nvidia-smi presence/absence can be simulated deterministically, instead of
// depending on whether the machine actually running this test has an NVIDIA
// GPU (CI/sandboxes generally don't).
func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	old := lookPath
	lookPath = fn
	t.Cleanup(func() { lookPath = old })
}

func TestNvidiaCollectorAvailableWhenOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/bin/nvidia-smi", nil })
	if !(nvidiaCollector{}).Available(context.Background()) {
		t.Error("expected Available()=true when nvidia-smi resolves on PATH")
	}
}

func TestNvidiaCollectorUnavailableWhenNotOnPath(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if (nvidiaCollector{}).Available(context.Background()) {
		t.Error("expected Available()=false when nvidia-smi is not on PATH")
	}
}

func TestDetectGPUCollectorPicksNvidiaWhenAvailable(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/bin/nvidia-smi", nil })
	c := detectGPUCollector(context.Background())
	if c.Name() != "nvidia" {
		t.Errorf("Name() = %q, want nvidia", c.Name())
	}
}

// TestDetectGPUCollectorFallsBackToNullObject verifies a host with no
// recognized GPU backend gets the explicit noGPUCollector rather than a nil
// GPUCollector - Scheduler never has to nil-check its gpu field.
func TestDetectGPUCollectorFallsBackToNullObject(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	c := detectGPUCollector(context.Background())
	if c.Name() != "none" {
		t.Errorf("Name() = %q, want none", c.Name())
	}
	if _, err := c.Collect(context.Background()); err == nil {
		t.Error("expected noGPUCollector.Collect to always error")
	}
}

const sampleNvidiaSMIXML = `<?xml version="1.0" ?>
<nvidia_smi_log>
	<gpu id="00000000:01:00.0">
		<fan_speed>52 %</fan_speed>
		<fb_memory_usage>
			<total>24576 MiB</total>
			<used>21504 MiB</used>
		</fb_memory_usage>
		<temperature>
			<gpu_temp>67 C</gpu_temp>
		</temperature>
		<power_readings>
			<power_draw>218.00 W</power_draw>
		</power_readings>
	</gpu>
</nvidia_smi_log>`

func TestParseNvidiaSMIXML(t *testing.T) {
	gpu, ok := parseNvidiaSMIXML([]byte(sampleNvidiaSMIXML))
	if !ok {
		t.Fatal("expected ok=true for valid XML")
	}
	if gpu.VRAMTotalMB != 24576 {
		t.Errorf("VRAMTotalMB = %d, want 24576", gpu.VRAMTotalMB)
	}
	if gpu.VRAMUsedMB != 21504 {
		t.Errorf("VRAMUsedMB = %d, want 21504", gpu.VRAMUsedMB)
	}
	if gpu.TemperatureC == nil || *gpu.TemperatureC != 67 {
		t.Errorf("TemperatureC = %v, want 67", gpu.TemperatureC)
	}
	if gpu.PowerWatts == nil || *gpu.PowerWatts != 218 {
		t.Errorf("PowerWatts = %v, want 218", gpu.PowerWatts)
	}
	if gpu.FanPercent == nil || *gpu.FanPercent != 52 {
		t.Errorf("FanPercent = %v, want 52", gpu.FanPercent)
	}
}

func TestParseNvidiaSMIXMLNoGPUs(t *testing.T) {
	_, ok := parseNvidiaSMIXML([]byte(`<?xml version="1.0" ?><nvidia_smi_log></nvidia_smi_log>`))
	if ok {
		t.Error("expected ok=false when no <gpu> elements are present")
	}
}

func TestParseNvidiaSMIXMLMalformed(t *testing.T) {
	_, ok := parseNvidiaSMIXML([]byte(`not xml at all`))
	if ok {
		t.Error("expected ok=false for malformed XML")
	}
}

// TestParseNvidiaSMIXMLMissingFanSpeed verifies a GPU without a reported fan
// speed (e.g. "N/A", common on some cards/drivers) omits FanPercent rather
// than reporting a fabricated 0% (R1).
func TestParseNvidiaSMIXMLMissingFanSpeed(t *testing.T) {
	xml := `<?xml version="1.0" ?>
<nvidia_smi_log>
	<gpu id="0">
		<fan_speed>N/A</fan_speed>
		<fb_memory_usage><total>100 MiB</total><used>50 MiB</used></fb_memory_usage>
		<temperature><gpu_temp>50 C</gpu_temp></temperature>
		<power_readings><power_draw>N/A</power_draw></power_readings>
		<gpu_power_readings><power_draw>75.00 W</power_draw></gpu_power_readings>
	</gpu>
</nvidia_smi_log>`
	gpu, ok := parseNvidiaSMIXML([]byte(xml))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if gpu.FanPercent != nil {
		t.Errorf("FanPercent should be nil for N/A, got %v", *gpu.FanPercent)
	}
	// power_readings is N/A, so gpu_power_readings should be the fallback.
	if gpu.PowerWatts == nil || *gpu.PowerWatts != 75 {
		t.Errorf("PowerWatts = %v, want 75 (fallback to gpu_power_readings)", gpu.PowerWatts)
	}
}
