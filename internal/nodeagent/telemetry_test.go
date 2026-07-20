package nodeagent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTelemetryMarshalSchema(t *testing.T) {
	temp := 67.0
	fan := 52.0
	power := 218.0
	core := 87.5
	cpu := 34.0
	lastUpdated := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tel := Telemetry{
		Agent: Agent{
			NodeID:          "a1b2c3d4-0000-4000-8000-000000000000",
			Version:         "v0.17.0",
			ProtocolVersion: ProtocolVersion,
			Platform:        "linux",
			Architecture:    "amd64",
		},
		Capabilities: []string{"status", "models.pull"},
		LastUpdated:  lastUpdated,
		GPU: &GPUBlock{
			Count:         1,
			Vendor:        "nvidia",
			DriverVersion: "535.183.01",
			CUDAVersion:   "12.2",
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
			CPUPercent:    &cpu,
			RAMUsedMB:     12000,
			RAMTotalMB:    65536,
			DiskFreeGB:    220,
			DiskTotalGB:   1000,
			Hostname:      "gpu-node-01",
			UptimeSeconds: 432000,
			BootTime:      1752883200,
		},
		Runtime: &RuntimeInfo{
			Name:       "ollama",
			Version:    "0.6.5",
			Status:     "up",
			WarmModels: []string{"llama3.1:8b"},
		},
		Health: Health{RuntimeReachable: true},
	}

	b, err := json.Marshal(tel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	agent, ok := decoded["agent"].(map[string]interface{})
	if !ok {
		t.Fatalf("agent field missing or wrong type: %v", decoded["agent"])
	}
	if agent["node_id"] != "a1b2c3d4-0000-4000-8000-000000000000" {
		t.Errorf("agent.node_id = %v", agent["node_id"])
	}
	if agent["version"] != "v0.17.0" {
		t.Errorf("agent.version = %v", agent["version"])
	}
	if agent["protocol_version"].(float64) != 1 {
		t.Errorf("agent.protocol_version = %v, want 1", agent["protocol_version"])
	}
	if agent["platform"] != "linux" {
		t.Errorf("agent.platform = %v, want linux", agent["platform"])
	}
	if agent["architecture"] != "amd64" {
		t.Errorf("agent.architecture = %v, want amd64", agent["architecture"])
	}
	if decoded["last_updated"] != lastUpdated.Format(time.RFC3339Nano) {
		t.Errorf("last_updated = %v, want %v", decoded["last_updated"], lastUpdated.Format(time.RFC3339Nano))
	}
	caps, ok := decoded["capabilities"].([]interface{})
	if !ok || len(caps) != 2 || caps[0] != "status" || caps[1] != "models.pull" {
		t.Errorf("capabilities = %v, want [status models.pull]", decoded["capabilities"])
	}

	gpu, ok := decoded["gpu"].(map[string]interface{})
	if !ok {
		t.Fatalf("gpu field missing or wrong type: %v", decoded["gpu"])
	}
	if gpu["vendor"] != "nvidia" {
		t.Errorf("gpu.vendor = %v, want nvidia", gpu["vendor"])
	}
	if gpu["driver_version"] != "535.183.01" {
		t.Errorf("gpu.driver_version = %v", gpu["driver_version"])
	}
	devices, ok := gpu["devices"].([]interface{})
	if !ok || len(devices) != 1 {
		t.Fatalf("gpu.devices = %v, want 1 device", gpu["devices"])
	}
	dev0 := devices[0].(map[string]interface{})
	if dev0["temperature_c"].(float64) != 67 {
		t.Errorf("gpu.devices[0].temperature_c = %v", dev0["temperature_c"])
	}
	if dev0["core_percent"].(float64) != 87.5 {
		t.Errorf("gpu.devices[0].core_percent = %v", dev0["core_percent"])
	}
	if dev0["vram_used_mb"].(float64) != 21504 {
		t.Errorf("gpu.devices[0].vram_used_mb = %v", dev0["vram_used_mb"])
	}

	host, ok := decoded["host"].(map[string]interface{})
	if !ok {
		t.Fatalf("host field missing or wrong type: %v", decoded["host"])
	}
	if host["cpu_percent"].(float64) != 34 {
		t.Errorf("host.cpu_percent = %v", host["cpu_percent"])
	}
	if host["ram_total_mb"].(float64) != 65536 {
		t.Errorf("host.ram_total_mb = %v", host["ram_total_mb"])
	}
	if host["disk_total_gb"].(float64) != 1000 {
		t.Errorf("host.disk_total_gb = %v", host["disk_total_gb"])
	}
	if host["hostname"] != "gpu-node-01" {
		t.Errorf("host.hostname = %v", host["hostname"])
	}

	runtimeBlock, ok := decoded["runtime"].(map[string]interface{})
	if !ok {
		t.Fatalf("runtime field missing or wrong type: %v", decoded["runtime"])
	}
	if runtimeBlock["name"] != "ollama" {
		t.Errorf("runtime.name = %v, want ollama", runtimeBlock["name"])
	}
	if runtimeBlock["version"] != "0.6.5" {
		t.Errorf("runtime.version = %v, want 0.6.5", runtimeBlock["version"])
	}
	if runtimeBlock["status"] != "up" {
		t.Errorf("runtime.status = %v, want up", runtimeBlock["status"])
	}

	health, ok := decoded["health"].(map[string]interface{})
	if !ok {
		t.Fatalf("health field missing or wrong type: %v", decoded["health"])
	}
	if health["runtime_reachable"] != true {
		t.Errorf("health.runtime_reachable = %v, want true", health["runtime_reachable"])
	}

	// No "mesh" block anywhere - the agent never reports mesh-owned
	// scheduler metrics (prefix/session-affinity hit rate, warm-residency
	// confidence). See node-agent.md's protocol v1 design notes.
	if _, present := decoded["mesh"]; present {
		t.Errorf("unexpected mesh block: %v", decoded["mesh"])
	}
}

// TestTelemetryOmitsUnknownFields verifies that a Telemetry with nil
// GPU/Host/Runtime (nothing measurable/detected on this platform/host)
// never fabricates zero-valued measurements (R1): the fields must be
// entirely absent from the JSON, not present with a 0.
func TestTelemetryOmitsUnknownFields(t *testing.T) {
	tel := Telemetry{Agent: Agent{Version: "v0.17.0", ProtocolVersion: ProtocolVersion}}
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
	if _, present := decoded["runtime"]; present {
		t.Errorf("runtime should be omitted when not detected, got %v", decoded["runtime"])
	}
	// Health is always present (a plain struct, not a pointer) - even its
	// zero value (runtime_reachable: false) is honest information, not a
	// fabricated measurement.
	health, ok := decoded["health"].(map[string]interface{})
	if !ok {
		t.Fatalf("health should always be present, got %v", decoded["health"])
	}
	if health["runtime_reachable"] != false {
		t.Errorf("health.runtime_reachable = %v, want false", health["runtime_reachable"])
	}
}

// TestGPUInfoOmitsUnmeasuredFields checks that individual GPU device fields
// the node couldn't measure (nil pointers) are omitted rather than
// serialized as 0/null, per R1 - "never fabricate a measurement."
func TestGPUInfoOmitsUnmeasuredFields(t *testing.T) {
	gpu := GPUInfo{Index: 0, VRAMUsedMB: 100, VRAMTotalMB: 200}
	b, err := json.Marshal(gpu)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"core_percent", "temperature_c", "fan_percent", "power_watts"} {
		if _, present := decoded[field]; present {
			t.Errorf("%s should be omitted when nil, got %v", field, decoded[field])
		}
	}
}
