package marboragent

import (
	"strings"
	"testing"
)

func TestRenderPrometheusDerivedFromTelemetry(t *testing.T) {
	temp := 67.0
	fan := 52.0
	power := 218.0
	core := 87.0
	cpu := 34.0
	tel := Telemetry{
		Agent: Agent{
			NodeID:          "test-node-id",
			Version:         "v0.16.0",
			ProtocolVersion: 1,
			Platform:        "linux",
			Architecture:    "amd64",
		},
		Capabilities: []string{"status", "models.pull"},
		GPU: &GPUBlock{
			Count:  1,
			Vendor: "nvidia",
			Devices: []GPUInfo{
				{
					Index:        0,
					Vendor:       "nvidia",
					CorePercent:  &core,
					TemperatureC: &temp,
					FanPercent:   &fan,
					PowerWatts:   &power,
					VRAMUsedMB:   21504,
					VRAMTotalMB:  24576,
				},
			},
		},
		Host: &HostTelemetry{
			CPUPercent: &cpu,
			RAMUsedMB:  12000,
			DiskFreeGB: 220,
		},
		Runtime: &RuntimeInfo{Name: "ollama"},
	}

	out := RenderPrometheus(tel)

	for _, want := range []string{
		`marboragent_gpu_temperature_celsius{gpu="0"} 67`,
		`marboragent_gpu_fan_percent{gpu="0"} 52`,
		`marboragent_gpu_power_watts{gpu="0"} 218`,
		`marboragent_gpu_core_percent{gpu="0"} 87`,
		`marboragent_gpu_vram_used_mb{gpu="0"} 21504`,
		`marboragent_gpu_vram_total_mb{gpu="0"} 24576`,
		"marboragent_gpu_count 1",
		"marboragent_host_cpu_percent 34",
		"marboragent_host_ram_used_mb 12000",
		"marboragent_host_disk_free_gb 220",
		"marboragent_protocol_version 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prometheus output missing %q\nfull output:\n%s", want, out)
		}
	}

	// Every metric must carry HELP/TYPE per the Prometheus text format.
	if !strings.Contains(out, "# HELP marboragent_gpu_temperature_celsius") {
		t.Error("missing HELP line for gpu_temperature_celsius")
	}
	if !strings.Contains(out, "# TYPE marboragent_gpu_temperature_celsius gauge") {
		t.Error("missing TYPE line for gpu_temperature_celsius")
	}

	// marboragent_info carries the agent's identity/string metadata as labels
	// (the standard Prometheus "info metric" convention) so a mixed fleet
	// can be grouped/filtered by node identity, version, platform, GPU
	// vendor, or detected runtime from Prometheus/Grafana directly, not
	// just from /v1/status JSON.
	if !strings.Contains(out, `marboragent_info{node_id="test-node-id",agent_version="v0.16.0",platform="linux",architecture="amd64",gpu_vendor="nvidia",runtime="ollama",capabilities="status,models.pull"} 1`) {
		t.Errorf("missing or wrong marboragent_info line:\n%s", out)
	}
}

// TestRenderPrometheusOmitsUnknownFields verifies that fields absent from
// the JSON telemetry (nil GPU/Host/Runtime, or nil sub-fields) are simply
// not rendered as metric lines, rather than rendered as a fabricated 0 -
// the Prometheus endpoint must never claim a measurement that wasn't taken.
func TestRenderPrometheusOmitsUnknownFields(t *testing.T) {
	tel := Telemetry{Agent: Agent{Version: "v0.16.0", ProtocolVersion: 1}}
	out := RenderPrometheus(tel)
	for _, absent := range []string{
		"marboragent_gpu_temperature_celsius",
		"marboragent_gpu_fan_percent",
		"marboragent_gpu_power_watts",
		"marboragent_gpu_core_percent",
		"marboragent_gpu_vram_used_mb",
		"marboragent_gpu_vram_total_mb",
		"marboragent_gpu_count",
		"marboragent_host_cpu_percent",
		"marboragent_host_ram_used_mb",
		"marboragent_host_disk_free_gb",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("expected %q to be absent when telemetry has no GPU/Host data, but it was rendered:\n%s", absent, out)
		}
	}
	// Protocol version is always known (it's a constant, not a measurement).
	if !strings.Contains(out, "marboragent_protocol_version 1") {
		t.Error("protocol_version metric should always be present")
	}
	// marboragent_info should report gpu_vendor="" (empty, not a magic "none"
	// string) and an empty runtime label when nothing was detected - never
	// omitted or fabricated.
	if !strings.Contains(out, `gpu_vendor=""`) {
		t.Errorf("expected gpu_vendor=\"\" when no GPU block is present:\n%s", out)
	}
}
