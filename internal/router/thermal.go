package router

import (
	"fmt"
	"log"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// scopedThermalReadingLocked returns the hottest temperature among the
// devices an agent reported for n, limited to n's GPU scope: the operator's
// declared indices, else a verified detected group. With no scope, or a scope
// that matches none of the reported devices, every device on the host counts
// (the same fallback placement uses). Devices that report no temperature are
// skipped; when none in scope reports one the reading is unknown (ok false),
// never borrowed from a device outside the scope. scopeApplied reports
// whether the scope narrowed the devices (false means the host-wide fallback
// was used). Caller holds n.mu.
func scopedThermalReadingLocked(n *NodeState, devices []marboragent.GPUInfo) (tempC float64, device int, scopeApplied, ok bool) {
	scoped, scopeApplied := scopeGPUsForPlacement(devices, effectiveGPUIndicesLocked(n))
	for _, d := range scoped {
		if d.TemperatureC == nil {
			continue
		}
		if !ok || *d.TemperatureC > tempC {
			tempC, device, ok = *d.TemperatureC, d.Index, true
		}
	}
	return tempC, device, scopeApplied, ok
}

// trackAgentThermalLocked counts one agent poll toward the thermal watchdog
// and returns a non-empty description of the triggering reading when n should
// now be drained. An unknown reading (no GPU block, no devices, no
// temperature in scope) resets the streak and never drains. Nothing is counted
// while the local nvidia-smi is the node's source (the local path owns the
// counter then) or when the watchdog is off. Caller holds n.mu; the drain
// itself must run after n.mu is released (see thermalDrain).
func (r *Router) trackAgentThermalLocked(n *NodeState, gpu *marboragent.GPUBlock) string {
	if n.VRAMSource == "nvidia" || !r.thermalWatchdog.Enabled || r.thermalWatchdog.MaxTempCelsius <= 0 {
		return ""
	}
	var devices []marboragent.GPUInfo
	if gpu != nil {
		devices = gpu.Devices
	}
	temp, device, scopeApplied, ok := scopedThermalReadingLocked(n, devices)
	if !ok || temp < r.thermalWatchdog.MaxTempCelsius {
		n.ThermalBreaches = 0
		return ""
	}
	n.ThermalBreaches++
	if n.ThermalBreaches >= max(r.thermalWatchdog.ConsecutiveBreaches, 1) && !n.Draining {
		scope := "node GPU scope"
		if !scopeApplied {
			scope = "no GPU scope known, whole host"
		}
		return fmt.Sprintf("agent GPU %d at %.1f°C, %s", device, temp, scope)
	}
	return ""
}

// thermalDrain drains the node through the same path as the local watchdog
// and persists it so a restart does not silently undrain a still-hot GPU.
// detail names the triggering device and reading. Must not be called with
// any node lock held (DrainNode takes r.mu then n.mu).
func (r *Router) thermalDrain(nodeName, detail string) {
	if !r.DrainNode(nodeName, "thermal", 0) {
		return // node removed since the poll: nothing to persist or report
	}
	if r.store != nil {
		if err := r.store.SetNodeDrain(nodeName, true, "thermal", 0); err != nil {
			log.Printf("thermal watchdog: persist drain for %s: %v", nodeName, err)
		}
	}
	log.Printf("thermal watchdog: node %s auto-drained after %d consecutive polls at/above %.1f°C (%s)",
		nodeName, r.thermalWatchdog.ConsecutiveBreaches, r.thermalWatchdog.MaxTempCelsius, detail)
}
