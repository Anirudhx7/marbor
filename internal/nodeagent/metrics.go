package nodeagent

import (
	"fmt"
	"strconv"
	"strings"
)

// RenderPrometheus generates a Prometheus text-format exposition of t, for
// operators who already run Grafana/Prometheus scraping externally. Per the
// build spec, /v1/status (JSON) is the canonical protocol and this is
// derived FROM it - not a second collection path - so the two endpoints can
// never disagree. Left unversioned (no /v1 prefix) per Prometheus's own
// scrape-target convention.
func RenderPrometheus(t Telemetry) string {
	var b strings.Builder

	writeGauge := func(name, help string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatFloat(value))
	}
	writeGaugeLabeled := func(name, help, labelName, labelValue string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s{%s=%q} %s\n", name, help, name, name, labelName, labelValue, formatFloat(value))
	}

	// Empty, not the literal "none" - consistent with every other
	// not-detected field on this wire format (empty string means "unknown/
	// not applicable", the same convention agent_poll.go/admin.go already
	// use for AgentGPUVendor when no GPU backend is selected on this host).
	gpuVendor := ""
	if t.GPU != nil {
		g := t.GPU
		if g.Vendor != "" {
			gpuVendor = g.Vendor
		}
		for _, d := range g.Devices {
			idx := strconv.Itoa(d.Index)
			if d.CorePercent != nil {
				writeGaugeLabeled("nodeagent_gpu_core_percent", "GPU compute utilization as a percent.", "gpu", idx, *d.CorePercent)
			}
			if d.TemperatureC != nil {
				writeGaugeLabeled("nodeagent_gpu_temperature_celsius", "GPU temperature in Celsius.", "gpu", idx, *d.TemperatureC)
			}
			if d.FanPercent != nil {
				writeGaugeLabeled("nodeagent_gpu_fan_percent", "GPU fan speed as a percent of max.", "gpu", idx, *d.FanPercent)
			}
			if d.PowerWatts != nil {
				writeGaugeLabeled("nodeagent_gpu_power_watts", "GPU power draw in watts.", "gpu", idx, *d.PowerWatts)
			}
			if d.VRAMUsedMB > 0 {
				writeGaugeLabeled("nodeagent_gpu_vram_used_mb", "GPU VRAM used in MB.", "gpu", idx, float64(d.VRAMUsedMB))
			}
			if d.VRAMTotalMB > 0 {
				writeGaugeLabeled("nodeagent_gpu_vram_total_mb", "GPU VRAM total in MB.", "gpu", idx, float64(d.VRAMTotalMB))
			}
		}
		if g.Count > 0 {
			writeGauge("nodeagent_gpu_count", "Total GPU count on host.", float64(g.Count))
		}
	}

	if t.Host != nil {
		h := t.Host
		if h.CPUPercent != nil {
			writeGauge("nodeagent_host_cpu_percent", "Host CPU utilization as a percent.", *h.CPUPercent)
		}
		if h.RAMUsedMB > 0 {
			writeGauge("nodeagent_host_ram_used_mb", "Host RAM used in MB.", float64(h.RAMUsedMB))
		}
		if h.RAMTotalMB > 0 {
			writeGauge("nodeagent_host_ram_total_mb", "Host RAM total in MB.", float64(h.RAMTotalMB))
		}
		if h.DiskFreeGB > 0 {
			writeGauge("nodeagent_host_disk_free_gb", "Host disk free space in GB.", h.DiskFreeGB)
		}
		if h.DiskTotalGB > 0 {
			writeGauge("nodeagent_host_disk_total_gb", "Host disk total space in GB.", h.DiskTotalGB)
		}
		if h.UptimeSeconds > 0 {
			writeGauge("nodeagent_host_uptime_seconds", "Seconds since last boot.", float64(h.UptimeSeconds))
		}
	}

	fmt.Fprintf(&b, "# HELP nodeagent_protocol_version The Node Agent Protocol version served by this agent.\n# TYPE nodeagent_protocol_version gauge\nnodeagent_protocol_version %d\n", t.Agent.ProtocolVersion)

	runtimeName := ""
	if t.Runtime != nil {
		runtimeName = t.Runtime.Name
	}

	// nodeagent_info follows the common Prometheus "info metric" convention
	// (node_exporter's node_uname_info, kube_state_metrics' kube_pod_info,
	// ...): string metadata that doesn't fit a numeric gauge is carried as
	// labels on a constant value-1 series, so an operator's existing
	// Prometheus/Grafana setup can group/filter a mixed fleet by node
	// identity, agent version, platform, GPU vendor, or detected runtime
	// without needing a separate side-channel for that information.
	fmt.Fprintf(&b,
		"# HELP nodeagent_info Node Agent identity/build/platform/vendor metadata (value is always 1; read the labels).\n# TYPE nodeagent_info gauge\nnodeagent_info{node_id=%q,agent_version=%q,platform=%q,architecture=%q,gpu_vendor=%q,runtime=%q,capabilities=%q} 1\n",
		t.Agent.NodeID, t.Agent.Version, t.Agent.Platform, t.Agent.Architecture, gpuVendor, runtimeName, strings.Join(t.Capabilities, ","),
	)

	return b.String()
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}
