package marboragent

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
	<driver_version>535.183.01</driver_version>
	<cuda_version>12.2</cuda_version>
	<gpu id="00000000:01:00.0">
		<product_name>NVIDIA GeForce RTX 4090</product_name>
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
		<utilization>
			<gpu_util>87 %</gpu_util>
		</utilization>
	</gpu>
	<gpu id="00000000:02:00.0">
		<fan_speed>35 %</fan_speed>
		<fb_memory_usage>
			<total>24576 MiB</total>
			<used>4096 MiB</used>
		</fb_memory_usage>
		<temperature>
			<gpu_temp>42 C</gpu_temp>
		</temperature>
		<power_readings>
			<power_draw>48.00 W</power_draw>
		</power_readings>
		<utilization>
			<gpu_util>12 %</gpu_util>
		</utilization>
	</gpu>
</nvidia_smi_log>`

func TestParseNvidiaSMIXMLMultiGPU(t *testing.T) {
	block, ok := parseNvidiaSMIXML([]byte(sampleNvidiaSMIXML))
	if !ok {
		t.Fatal("expected ok=true for valid XML")
	}
	if block.Count != 2 {
		t.Fatalf("Count = %d, want 2", block.Count)
	}
	if block.DriverVersion != "535.183.01" {
		t.Errorf("DriverVersion = %q, want 535.183.01", block.DriverVersion)
	}
	if block.CUDAVersion != "12.2" {
		t.Errorf("CUDAVersion = %q, want 12.2", block.CUDAVersion)
	}
	if len(block.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2", len(block.Devices))
	}

	d0 := block.Devices[0]
	if d0.Index != 0 {
		t.Errorf("Devices[0].Index = %d, want 0", d0.Index)
	}
	if d0.Model != "NVIDIA GeForce RTX 4090" {
		t.Errorf("Devices[0].Model = %q, want NVIDIA GeForce RTX 4090", d0.Model)
	}
	if d0.VRAMTotalMB != 24576 || d0.VRAMUsedMB != 21504 {
		t.Errorf("Devices[0] VRAM = %d/%d, want 21504/24576", d0.VRAMUsedMB, d0.VRAMTotalMB)
	}
	if d0.TemperatureC == nil || *d0.TemperatureC != 67 {
		t.Errorf("Devices[0].TemperatureC = %v, want 67", d0.TemperatureC)
	}
	if d0.PowerWatts == nil || *d0.PowerWatts != 218 {
		t.Errorf("Devices[0].PowerWatts = %v, want 218", d0.PowerWatts)
	}
	if d0.FanPercent == nil || *d0.FanPercent != 52 {
		t.Errorf("Devices[0].FanPercent = %v, want 52", d0.FanPercent)
	}
	if d0.CorePercent == nil || *d0.CorePercent != 87 {
		t.Errorf("Devices[0].CorePercent = %v, want 87", d0.CorePercent)
	}

	d1 := block.Devices[1]
	if d1.Index != 1 {
		t.Errorf("Devices[1].Index = %d, want 1", d1.Index)
	}
	if d1.Model != "" {
		t.Errorf("Devices[1].Model = %q, want empty (no product_name reported)", d1.Model)
	}
	if d1.VRAMUsedMB != 4096 {
		t.Errorf("Devices[1].VRAMUsedMB = %d, want 4096", d1.VRAMUsedMB)
	}
	if d1.CorePercent == nil || *d1.CorePercent != 12 {
		t.Errorf("Devices[1].CorePercent = %v, want 12", d1.CorePercent)
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
	block, ok := parseNvidiaSMIXML([]byte(xml))
	if !ok {
		t.Fatal("expected ok=true")
	}
	dev := block.Devices[0]
	if dev.FanPercent != nil {
		t.Errorf("FanPercent should be nil for N/A, got %v", *dev.FanPercent)
	}
	// power_readings is N/A, so gpu_power_readings should be the fallback.
	if dev.PowerWatts == nil || *dev.PowerWatts != 75 {
		t.Errorf("PowerWatts = %v, want 75 (fallback to gpu_power_readings)", dev.PowerWatts)
	}
}

// TestParseNvidiaSMIXMLMissingTemperatureAndPower verifies that a card
// reporting "N/A" for temperature, and "N/A" on BOTH power sources, omits
// TemperatureC/PowerWatts entirely rather than reporting a fabricated 0°C /
// 0W (R1).
func TestParseNvidiaSMIXMLMissingTemperatureAndPower(t *testing.T) {
	xml := `<?xml version="1.0" ?>
<nvidia_smi_log>
	<gpu id="0">
		<fan_speed>N/A</fan_speed>
		<fb_memory_usage><total>100 MiB</total><used>50 MiB</used></fb_memory_usage>
		<temperature><gpu_temp>N/A</gpu_temp></temperature>
		<power_readings><power_draw>N/A</power_draw></power_readings>
		<gpu_power_readings><power_draw>N/A</power_draw></gpu_power_readings>
	</gpu>
</nvidia_smi_log>`
	block, ok := parseNvidiaSMIXML([]byte(xml))
	if !ok {
		t.Fatal("expected ok=true")
	}
	dev := block.Devices[0]
	if dev.TemperatureC != nil {
		t.Errorf("TemperatureC should be nil when gpu_temp is N/A, got %v", *dev.TemperatureC)
	}
	if dev.PowerWatts != nil {
		t.Errorf("PowerWatts should be nil when both power sources are N/A, got %v", *dev.PowerWatts)
	}
}

// TestParseNvidiaSMIXMLMissingCoreUtilization verifies a GPU without a
// reported utilization block omits CorePercent rather than a fabricated 0%.
func TestParseNvidiaSMIXMLMissingCoreUtilization(t *testing.T) {
	xml := `<?xml version="1.0" ?>
<nvidia_smi_log>
	<gpu id="0">
		<fb_memory_usage><total>100 MiB</total><used>50 MiB</used></fb_memory_usage>
	</gpu>
</nvidia_smi_log>`
	block, ok := parseNvidiaSMIXML([]byte(xml))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if block.Devices[0].CorePercent != nil {
		t.Errorf("CorePercent should be nil when utilization is absent, got %v", *block.Devices[0].CorePercent)
	}
}
