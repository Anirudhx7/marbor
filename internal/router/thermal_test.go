package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// newThermalTestRouter builds a single-node router whose node is treated as
// local (so nvidia-smi data attributes to it) and backed by a real /api/ps
// mock, with the given thermal watchdog config.
func newThermalTestRouter(t *testing.T, watchdog config.ThermalWatchdogConfig) (*Router, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	t.Cleanup(srv.Close)

	r := New(config.RoutingConfig{
		Strategy:        "warm-first",
		ThermalWatchdog: watchdog,
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL, Runtime: "ollama", NvidiaIndex: 0},
	}, nil)
	return r, srv
}

// TestThermalWatchdog_AutoDrainsAfterConsecutiveBreaches verifies that a node
// is drained via the existing DrainNode path only after the configured
// number of consecutive polls at/above the threshold - not sooner.
func TestThermalWatchdog_AutoDrainsAfterConsecutiveBreaches(t *testing.T) {
	r, _ := newThermalTestRouter(t, config.ThermalWatchdogConfig{
		Enabled:             true,
		MaxTempCelsius:      85,
		ConsecutiveBreaches: 3,
	})
	n := r.nodes[0]

	r.nvidiaMu.Lock()
	r.nvidiaCache[0] = GPUStats{TempCelsius: 90, VRAMTotalMB: 24576, VRAMUsedMB: 1000}
	r.nvidiaMu.Unlock()

	for i := 0; i < 2; i++ {
		r.pollNode(n)
		n.mu.RLock()
		draining := n.Draining
		n.mu.RUnlock()
		if draining {
			t.Fatalf("node drained too early, after only %d breach(es)", i+1)
		}
	}

	r.pollNode(n) // 3rd consecutive breach
	n.mu.RLock()
	draining := n.Draining
	n.mu.RUnlock()
	if !draining {
		t.Error("expected node to be auto-drained after 3 consecutive thermal breaches")
	}
}

// TestThermalWatchdog_CoolingResetsBreachCounter verifies that a poll below
// the threshold resets the consecutive-breach counter, so an intermittent
// spike never accumulates toward a drain.
func TestThermalWatchdog_CoolingResetsBreachCounter(t *testing.T) {
	r, _ := newThermalTestRouter(t, config.ThermalWatchdogConfig{
		Enabled:             true,
		MaxTempCelsius:      85,
		ConsecutiveBreaches: 3,
	})
	n := r.nodes[0]

	r.nvidiaMu.Lock()
	r.nvidiaCache[0] = GPUStats{TempCelsius: 90, VRAMTotalMB: 24576, VRAMUsedMB: 1000}
	r.nvidiaMu.Unlock()
	r.pollNode(n)
	r.pollNode(n)

	// Cools down for one poll.
	r.nvidiaMu.Lock()
	r.nvidiaCache[0] = GPUStats{TempCelsius: 70, VRAMTotalMB: 24576, VRAMUsedMB: 1000}
	r.nvidiaMu.Unlock()
	r.pollNode(n)

	// Two more breaches - would have hit 3 without the reset, but the
	// counter should only be at 2 now.
	r.nvidiaMu.Lock()
	r.nvidiaCache[0] = GPUStats{TempCelsius: 90, VRAMTotalMB: 24576, VRAMUsedMB: 1000}
	r.nvidiaMu.Unlock()
	r.pollNode(n)
	r.pollNode(n)

	n.mu.RLock()
	draining := n.Draining
	n.mu.RUnlock()
	if draining {
		t.Error("node should not be drained - the cooling poll should have reset the breach counter")
	}
}

// TestThermalWatchdog_DisabledNeverDrains verifies that with the watchdog
// disabled (the default), sustained overheat never triggers a drain.
func TestThermalWatchdog_DisabledNeverDrains(t *testing.T) {
	r, _ := newThermalTestRouter(t, config.ThermalWatchdogConfig{Enabled: false})
	n := r.nodes[0]

	r.nvidiaMu.Lock()
	r.nvidiaCache[0] = GPUStats{TempCelsius: 99, VRAMTotalMB: 24576, VRAMUsedMB: 1000}
	r.nvidiaMu.Unlock()

	for i := 0; i < 10; i++ {
		r.pollNode(n)
	}

	n.mu.RLock()
	draining := n.Draining
	n.mu.RUnlock()
	if draining {
		t.Error("watchdog disabled: node must never be auto-drained regardless of temperature")
	}
}
