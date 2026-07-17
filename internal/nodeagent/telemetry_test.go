package nodeagent

import (
	"encoding/json"
	"testing"
)

func TestTelemetryMarshalSchema(t *testing.T) {
	temp := 67.0
	fan := 52.0
	power := 218.0
	cpu := 34.0
	tel := Telemetry{
		SchemaVersion: SchemaVersion,
		AgentVersion:  "v0.16.0",
		GPU: &GPUTelemetry{
			TemperatureC: &temp,
			FanPercent:   &fan,
			PowerWatts:   &power,
			VRAMUsedMB:   21504,
			VRAMTotalMB:  24576,
		},
		Host: &HostTelemetry{
			CPUPercent: &cpu,
			RAMUsedMB:  12000,
			DiskFreeGB: 220,
		},
	}

	b, err := json.Marshal(tel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["schema_version"].(float64) != 1 {
		t.Errorf("schema_version = %v, want 1", decoded["schema_version"])
	}
	if decoded["agent_version"] != "v0.16.0" {
		t.Errorf("agent_version = %v", decoded["agent_version"])
	}
	gpu, ok := decoded["gpu"].(map[string]interface{})
	if !ok {
		t.Fatalf("gpu field missing or wrong type: %v", decoded["gpu"])
	}
	if gpu["temperature_c"].(float64) != 67 {
		t.Errorf("gpu.temperature_c = %v", gpu["temperature_c"])
	}
	if gpu["fan_percent"].(float64) != 52 {
		t.Errorf("gpu.fan_percent = %v", gpu["fan_percent"])
	}
	if gpu["vram_used_mb"].(float64) != 21504 {
		t.Errorf("gpu.vram_used_mb = %v", gpu["vram_used_mb"])
	}
	host, ok := decoded["host"].(map[string]interface{})
	if !ok {
		t.Fatalf("host field missing or wrong type: %v", decoded["host"])
	}
	if host["cpu_percent"].(float64) != 34 {
		t.Errorf("host.cpu_percent = %v", host["cpu_percent"])
	}
	if host["disk_free_gb"].(float64) != 220 {
		t.Errorf("host.disk_free_gb = %v", host["disk_free_gb"])
	}

	// No "mesh" block in v1 (explicitly out of scope - see build spec).
	if _, present := decoded["mesh"]; present {
		t.Errorf("unexpected mesh block in v1 schema: %v", decoded["mesh"])
	}
}

// TestTelemetryOmitsUnknownFields verifies that a Telemetry with nil GPU/Host
// (nothing measurable on this platform/host) never fabricates zero-valued
// measurements (R1): the fields must be entirely absent from the JSON, not
// present with a 0.
func TestTelemetryOmitsUnknownFields(t *testing.T) {
	tel := Telemetry{SchemaVersion: SchemaVersion, AgentVersion: "v0.16.0"}
	b, err := json.Marshal(tel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["gpu"]; present {
		t.Errorf("gpu should be omitted when nil, got %v", decoded["gpu"])
	}
	if _, present := decoded["host"]; present {
		t.Errorf("host should be omitted when nil, got %v", decoded["host"])
	}
}

// TestGPUTelemetryOmitsUnmeasuredFields checks that individual GPU fields
// the node couldn't measure (nil pointers) are omitted rather than
// serialized as 0/null, per R1 - "never fabricate a measurement."
func TestGPUTelemetryOmitsUnmeasuredFields(t *testing.T) {
	gpu := GPUTelemetry{VRAMUsedMB: 100, VRAMTotalMB: 200}
	b, err := json.Marshal(gpu)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"temperature_c", "fan_percent", "power_watts"} {
		if _, present := decoded[field]; present {
			t.Errorf("%s should be omitted when nil, got %v", field, decoded[field])
		}
	}
}
