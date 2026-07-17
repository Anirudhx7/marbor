// Package nodeagent implements the ollama-mesh Node Agent: a small HTTP
// server that runs on a GPU node and reports GPU/host telemetry back to the
// mesh on its existing poll cycle. See .local/specs/node-agent.md for the
// full design (pull-only transport, versioned JSON schema, per-node opaque
// bearer token).
package nodeagent

// SchemaVersion is the current /telemetry JSON schema version. New fields
// added to Telemetry/GPUTelemetry/HostTelemetry must be optional (nil/omitted
// means "unknown", never fabricated - R1) so an older agent talking to a
// newer mesh, or vice versa, never breaks (R7 discipline extended to this
// wire schema).
const SchemaVersion = 1

// Telemetry is the canonical, versioned JSON payload served at GET /telemetry.
// GET /metrics (Prometheus text format) is generated from this same struct,
// not a second collection path.
type Telemetry struct {
	SchemaVersion int            `json:"schema_version"`
	AgentVersion  string         `json:"agent_version,omitempty"`
	GPU           *GPUTelemetry  `json:"gpu,omitempty"`
	Host          *HostTelemetry `json:"host,omitempty"`
}

// GPUTelemetry holds nvidia-smi-derived GPU stats. Every field the node
// can't measure is nil/zero rather than fabricated (R1); consumers must
// treat a nil pointer (or a zero VRAM value) as "unknown", never a real
// measurement of zero.
type GPUTelemetry struct {
	TemperatureC *float64 `json:"temperature_c,omitempty"`
	FanPercent   *float64 `json:"fan_percent,omitempty"`
	PowerWatts   *float64 `json:"power_watts,omitempty"`
	VRAMUsedMB   int64    `json:"vram_used_mb,omitempty"`
	VRAMTotalMB  int64    `json:"vram_total_mb,omitempty"`
}

// HostTelemetry holds stdlib-only host stats (CPU/RAM/disk). Fields that
// can't be measured on the current platform without a new dependency are
// omitted rather than guessed (R1, and Architecture Law: zero external
// dependencies).
type HostTelemetry struct {
	CPUPercent *float64 `json:"cpu_percent,omitempty"`
	RAMUsedMB  int64    `json:"ram_used_mb,omitempty"`
	DiskFreeGB float64  `json:"disk_free_gb,omitempty"`
}

// Collect gathers a fresh Telemetry snapshot: GPU stats via nvidia-smi (nil
// if nvidia-smi is unavailable/fails) and host stats via platform-specific
// stdlib/syscall reads (nil fields where unsupported on this platform).
func Collect(agentVersion string) Telemetry {
	t := Telemetry{
		SchemaVersion: SchemaVersion,
		AgentVersion:  agentVersion,
	}
	if gpu, ok := collectGPU(); ok {
		t.GPU = &gpu
	}
	t.Host = collectHost()
	return t
}
