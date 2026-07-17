package nodeagent

import (
	"fmt"
	"strings"
)

// RenderPrometheus generates a Prometheus text-format exposition of t, for
// operators who already run Grafana/Prometheus scraping externally. Per the
// build spec, /telemetry (JSON) is the canonical schema and this is derived
// FROM it - not a second collection path - so the two endpoints can never
// disagree.
func RenderPrometheus(t Telemetry) string {
	var b strings.Builder

	writeGauge := func(name, help string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatFloat(value))
	}

	if t.GPU != nil {
		g := t.GPU
		if g.TemperatureC != nil {
			writeGauge("nodeagent_gpu_temperature_celsius", "GPU temperature in Celsius.", *g.TemperatureC)
		}
		if g.FanPercent != nil {
			writeGauge("nodeagent_gpu_fan_percent", "GPU fan speed as a percent of max.", *g.FanPercent)
		}
		if g.PowerWatts != nil {
			writeGauge("nodeagent_gpu_power_watts", "GPU power draw in watts.", *g.PowerWatts)
		}
		if g.VRAMUsedMB > 0 {
			writeGauge("nodeagent_gpu_vram_used_mb", "GPU VRAM used in MB.", float64(g.VRAMUsedMB))
		}
		if g.VRAMTotalMB > 0 {
			writeGauge("nodeagent_gpu_vram_total_mb", "GPU VRAM total in MB.", float64(g.VRAMTotalMB))
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
		if h.DiskFreeGB > 0 {
			writeGauge("nodeagent_host_disk_free_gb", "Host disk free space in GB.", h.DiskFreeGB)
		}
	}

	fmt.Fprintf(&b, "# HELP nodeagent_schema_version The /telemetry JSON schema version served by this agent.\n# TYPE nodeagent_schema_version gauge\nnodeagent_schema_version %d\n", t.SchemaVersion)

	return b.String()
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}
