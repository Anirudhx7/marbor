package nodeagent

import "testing"

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
