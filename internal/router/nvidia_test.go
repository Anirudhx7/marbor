package router

import "testing"

var fixtureXML = []byte(`<?xml version="1.0" ?>
<nvidia_smi_log>
  <gpu id="GPU-abc123">
    <product_name>NVIDIA GeForce RTX 4090</product_name>
    <fb_memory_usage>
      <total>24564 MiB</total>
      <used>8192 MiB</used>
      <free>16372 MiB</free>
    </fb_memory_usage>
    <temperature>
      <gpu_temp>65 C</gpu_temp>
    </temperature>
    <power_readings>
      <power_draw>180.50 W</power_draw>
    </power_readings>
  </gpu>
  <gpu id="GPU-def456">
    <product_name>NVIDIA GeForce RTX 3090</product_name>
    <fb_memory_usage>
      <total>24268 MiB</total>
      <used>2048 MiB</used>
      <free>22220 MiB</free>
    </fb_memory_usage>
    <temperature>
      <gpu_temp>42 C</gpu_temp>
    </temperature>
    <gpu_power_readings>
      <power_draw>95.20 W</power_draw>
    </gpu_power_readings>
  </gpu>
</nvidia_smi_log>`)

func TestParseNvidiaSMIXML_GPU0(t *testing.T) {
	stats, ok := parseNvidiaSMIXML(fixtureXML, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if stats.VRAMTotalMB != 24564 {
		t.Errorf("VRAMTotalMB = %d, want 24564", stats.VRAMTotalMB)
	}
	if stats.VRAMUsedMB != 8192 {
		t.Errorf("VRAMUsedMB = %d, want 8192", stats.VRAMUsedMB)
	}
	if stats.TempCelsius != 65.0 {
		t.Errorf("TempCelsius = %f, want 65.0", stats.TempCelsius)
	}
	if stats.PowerDrawW != 180.50 {
		t.Errorf("PowerDrawW = %f, want 180.50", stats.PowerDrawW)
	}
}

func TestParseNvidiaSMIXML_GPU1_AltPowerTag(t *testing.T) {
	stats, ok := parseNvidiaSMIXML(fixtureXML, 1)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if stats.VRAMTotalMB != 24268 {
		t.Errorf("VRAMTotalMB = %d, want 24268", stats.VRAMTotalMB)
	}
	if stats.TempCelsius != 42.0 {
		t.Errorf("TempCelsius = %f, want 42.0", stats.TempCelsius)
	}
	if stats.PowerDrawW != 95.20 {
		t.Errorf("PowerDrawW = %f, want 95.20", stats.PowerDrawW)
	}
}

func TestParseNvidiaSMIXML_OutOfRange(t *testing.T) {
	_, ok := parseNvidiaSMIXML(fixtureXML, 5)
	if ok {
		t.Error("expected ok=false for out-of-range gpu index")
	}
}

func TestParseNvidiaSMIXML_InvalidXML(t *testing.T) {
	_, ok := parseNvidiaSMIXML([]byte("not xml"), 0)
	if ok {
		t.Error("expected ok=false for invalid XML")
	}
}
