package router

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

func thermalF(v float64) *float64 { return &v }

// agentThermalRouter builds a router whose nodes all sit on one remote host,
// with the thermal watchdog at 85C and the given consecutive-breach count.
func agentThermalRouter(t *testing.T, consecutive int, names ...string) (*Router, []*NodeState) {
	t.Helper()
	cfgs := make([]config.NodeConfig, 0, len(names))
	for i, name := range names {
		cfgs = append(cfgs, config.NodeConfig{Name: name, URL: fmt.Sprintf("http://10.9.9.9:%d", 11434+i), Runtime: "ollama"})
	}
	r := New(config.RoutingConfig{
		Strategy:        "warm-first",
		ThermalWatchdog: config.ThermalWatchdogConfig{Enabled: true, MaxTempCelsius: 85, ConsecutiveBreaches: consecutive},
	}, cfgs, nil)
	return r, r.nodes
}

func thermalTel(temps ...*float64) marboragent.Telemetry {
	devs := make([]marboragent.GPUInfo, len(temps))
	for i, tc := range temps {
		devs[i] = marboragent.GPUInfo{Index: i, TemperatureC: tc, VRAMTotalMB: 24000}
	}
	return marboragent.Telemetry{GPU: &marboragent.GPUBlock{Count: len(devs), Devices: devs}}
}

func thermalDraining(n *NodeState) (bool, string) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Draining, n.DrainedReason
}

func thermalBreachCount(n *NodeState) int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ThermalBreaches
}

func TestThermalWatchdog_AgentTempDrainsAfterConsecutiveBreaches(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	for i := 1; i <= 2; i++ {
		r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
		if d, _ := thermalDraining(n); d {
			t.Fatalf("drained after only %d breach(es)", i)
		}
	}
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	if d, reason := thermalDraining(n); !d || reason != "thermal" {
		t.Fatalf("draining=%v reason=%q, want true/thermal", d, reason)
	}
}

func TestThermalWatchdog_AgentDrainPersistsAndLogsDevice(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	st := openWarmTestStore(t)
	r.SetStore(st)
	buf := captureLog(t)

	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(60), thermalF(91.5)))

	states, err := st.NodeDrainStates()
	if err != nil {
		t.Fatal(err)
	}
	if !states["a"].Draining || states["a"].Reason != "thermal" {
		t.Errorf("persisted = %+v", states["a"])
	}
	if out := buf.String(); !strings.Contains(out, "agent GPU 1 at 91.5°C, no GPU scope known, whole host") {
		t.Errorf("log %q does not name the triggering device and reading", out)
	}
}

func TestThermalWatchdog_AgentUsesHottestInScopeOnly(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "hot", "cool")
	ns[0].DeclaredGPUIndices = []int{1}
	ns[1].DeclaredGPUIndices = []int{0}
	tel := thermalTel(thermalF(60), thermalF(95))
	buf := captureLog(t)
	for _, n := range ns {
		r.applyAgentTelemetryForHost(n, tel, 2)
	}
	if !strings.Contains(buf.String(), "agent GPU 1 at 95.0°C, node GPU scope") {
		t.Errorf("log %q should name the card and the scoped rule", buf.String())
	}
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("node scoped to the hot card should drain")
	}
	if d, _ := thermalDraining(ns[1]); d {
		t.Error("node scoped to the cool card must not drain")
	}
	ns[0].mu.RLock()
	temp := ns[0].Temperature
	ns[0].mu.RUnlock()
	if temp == nil || *temp != 60 {
		t.Errorf("display Temperature must stay device 0, got %v", temp)
	}
}

func TestThermalWatchdog_AgentUnscopedMultiMemberHostDrainsAll(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "x", "y")
	tel := thermalTel(thermalF(60), thermalF(95))
	for _, n := range ns {
		r.applyAgentTelemetryForHost(n, tel, 2)
	}
	for _, n := range ns {
		if d, _ := thermalDraining(n); !d {
			t.Errorf("%s: no scope known, host max applies", n.Name)
		}
	}
}

func TestThermalWatchdog_AgentVerifiedDetectedScope(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	n := ns[0]
	tel := thermalTel(thermalF(60), thermalF(95))
	tel.Deployments = []marboragent.DeploymentReport{{
		Port: portOfOrDefault(n.URL), GPUGroup: []int{0},
		GPUScope: &marboragent.GPUScope{Indices: []int{0}, CrossChecked: true},
	}}
	r.applyAgentTelemetry(n, tel)
	if d, _ := thermalDraining(n); d {
		t.Error("verified scope [0] is cool, must not drain on card 1")
	}
	tel.Deployments[0].GPUScope = &marboragent.GPUScope{Indices: []int{0}, CrossChecked: false}
	r.applyAgentTelemetry(n, tel)
	if d, _ := thermalDraining(n); !d {
		t.Error("unverified scope must fall back to host max and drain")
	}
}

func TestThermalWatchdog_AgentScopeMatchingNoDeviceUsesHostMax(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	ns[0].DeclaredGPUIndices = []int{7}
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(95)))
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("scope matching no device falls back to host max")
	}
}

func TestThermalWatchdog_AgentPartialNilUsesMaxOfKnown(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	r.applyAgentTelemetry(ns[0], thermalTel(nil, thermalF(90)))
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("max over devices that report a reading should drain")
	}
}

func TestThermalWatchdog_AgentScopedDevicesWithoutReadingNeverBorrow(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	ns[0].DeclaredGPUIndices = []int{0}
	r.applyAgentTelemetry(ns[0], thermalTel(nil, thermalF(95)))
	if d, _ := thermalDraining(ns[0]); d {
		t.Error("in-scope card reports nothing: unknown, must not borrow a sibling reading")
	}
}

func TestThermalWatchdog_AgentUnknownResetsAndNeverDrains(t *testing.T) {
	cases := []struct {
		name    string
		unknown marboragent.Telemetry
	}{
		{"nil temp", thermalTel(nil)},
		{"no devices", thermalTel()},
		{"no gpu block", marboragent.Telemetry{}},
		{"all nil multi", thermalTel(nil, nil)},
	}
	for _, tc := range cases {
		unknown := tc.unknown
		t.Run(tc.name, func(t *testing.T) {
			r, ns := agentThermalRouter(t, 3, "a")
			n := ns[0]
			r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
			r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
			r.applyAgentTelemetry(n, unknown)
			if b := thermalBreachCount(n); b != 0 {
				t.Errorf("breaches = %d, want 0 after unknown reading", b)
			}
			r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
			if d, _ := thermalDraining(n); d {
				t.Error("unknown reading must reset the streak")
			}
		})
	}
}

func TestThermalWatchdog_AgentCoolingResets(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(70)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	if d, _ := thermalDraining(n); d {
		t.Error("a cool poll must reset the streak")
	}
}

func TestThermalWatchdog_AgentSkippedWhenLocalNvidia(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	n := ns[0]
	n.mu.Lock()
	n.VRAMSource = "nvidia"
	n.mu.Unlock()
	r.applyAgentTelemetry(n, thermalTel(thermalF(99)))
	if d, _ := thermalDraining(n); d || thermalBreachCount(n) != 0 {
		t.Error("agent temperature must not count while the local nvidia-smi is the source")
	}
}

func TestThermalWatchdog_AgentDisabledNeverDrains(t *testing.T) {
	for i, cfg := range []config.ThermalWatchdogConfig{
		{Enabled: false, MaxTempCelsius: 85, ConsecutiveBreaches: 1},
		{Enabled: true, MaxTempCelsius: 0, ConsecutiveBreaches: 1},
	} {
		cfg := cfg
		t.Run(fmt.Sprintf("cfg%d", i), func(t *testing.T) {
			r := New(config.RoutingConfig{Strategy: "warm-first", ThermalWatchdog: cfg},
				[]config.NodeConfig{{Name: "a", URL: "http://10.9.9.9:11434", Runtime: "ollama"}}, nil)
			r.applyAgentTelemetry(r.nodes[0], thermalTel(thermalF(99)))
			if d, _ := thermalDraining(r.nodes[0]); d || thermalBreachCount(r.nodes[0]) != 0 {
				t.Errorf("cfg %+v counted or drained", cfg)
			}
		})
	}
}

func TestThermalWatchdog_AgentManualDrainNotOverridden(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	r.DrainNode("a", "maintenance", 0)
	buf := captureLog(t)
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(99)))
	if _, reason := thermalDraining(ns[0]); reason != "maintenance" {
		t.Errorf("reason = %q, manual drain must stay", reason)
	}
	if strings.Contains(buf.String(), "thermal watchdog") {
		t.Error("no thermal drain log while already draining")
	}
}

func TestThermalWatchdog_AgentUndrainWhileHotRedrains(t *testing.T) {
	r, ns := agentThermalRouter(t, 2, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.UndrainNode("a")
	if b := thermalBreachCount(n); b < 2 {
		t.Errorf("breaches = %d, the drain must not reset the streak", b)
	}
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	if d, _ := thermalDraining(n); !d {
		t.Error("still-hot node must redrain on the next poll, like the local path")
	}
}

func TestThermalWatchdog_AgentFlapBelowFailureThresholdKeepsStreak(t *testing.T) {
	r, ns := agentThermalRouter(t, 2, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.agentUnreachable(n) // one dropped poll, below the failure threshold
	if b := thermalBreachCount(n); b != 1 {
		t.Fatalf("breaches = %d after one dropped poll, want 1", b)
	}
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	if d, _ := thermalDraining(n); !d {
		t.Error("streak must survive a single dropped poll")
	}
}

func TestThermalWatchdog_AgentGoneAndOversizeResetStreak(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	applyOversizeTelemetry(n)
	if b := thermalBreachCount(n); b != 0 {
		t.Errorf("oversize: breaches = %d, want 0", b)
	}
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	clearAgentTelemetry(n)
	if b := thermalBreachCount(n); b != 0 {
		t.Errorf("agent gone: breaches = %d, want 0", b)
	}
}

func TestThermalWatchdog_LocalStreakSurvivesAgentClears(t *testing.T) {
	_, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	n.mu.Lock()
	n.VRAMSource = "nvidia"
	n.ThermalBreaches = 2
	n.mu.Unlock()
	clearAgentTelemetry(n)
	applyOversizeTelemetry(n)
	if b := thermalBreachCount(n); b != 2 {
		t.Errorf("local streak = %d, want 2 untouched", b)
	}
	// Agent-less host whose local read just failed (VRAMSource moved off nvidia).
	n.mu.Lock()
	n.VRAMSource = "none"
	n.AgentPresent = false
	n.mu.Unlock()
	clearAgentTelemetry(n)
	if b := thermalBreachCount(n); b != 2 {
		t.Errorf("failed local read: streak = %d, want 2 (the agent was not the source)", b)
	}
}

func TestThermalWatchdog_AgentVRAMSourceFlipContinuesStreak(t *testing.T) {
	r, ns := agentThermalRouter(t, 2, "a")
	n := ns[0]
	n.mu.Lock()
	n.VRAMSource = "none" // the local read failed last tick
	n.ThermalBreaches = 1
	n.mu.Unlock()
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	if d, _ := thermalDraining(n); !d {
		t.Error("a hot agent reading continues the same node's streak")
	}
	n.mu.RLock()
	src := n.VRAMSource
	n.mu.RUnlock()
	if src != "agent" {
		t.Errorf("VRAMSource = %q, want agent after the flip", src)
	}
}

func TestThermalWatchdog_ConcurrentPollsAndUndrainsRace(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a", "b")
	var wg sync.WaitGroup
	for _, n := range ns {
		n := n
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.applyAgentTelemetryForHost(n, thermalTel(thermalF(90)), 2)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.UndrainNode(n.Name)
				applyOversizeTelemetry(n)
				clearAgentTelemetry(n)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				thermalDraining(n)
				thermalBreachCount(n)
			}
		}()
	}
	wg.Wait()
	// The drain path ran and nothing deadlocked: from a known undrained state
	// the next hot poll must drain.
	r.UndrainNode(ns[0].Name)
	r.applyAgentTelemetryForHost(ns[0], thermalTel(thermalF(90)), 2)
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("final hot poll should leave the node draining")
	}
}

func TestThermalWatchdog_AgentPicksHottestRegardlessOfPositionOrIndex(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "first", "middle", "gap")
	// Hot card first, then cooler cards: the max must not be first-known or last.
	first := marboragent.Telemetry{GPU: &marboragent.GPUBlock{Devices: []marboragent.GPUInfo{
		{Index: 0, TemperatureC: thermalF(95)}, {Index: 1, TemperatureC: thermalF(60)}, {Index: 2, TemperatureC: thermalF(70)}}}}
	r.applyAgentTelemetry(ns[0], first)
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("hot first card must count")
	}
	mid := marboragent.Telemetry{GPU: &marboragent.GPUBlock{Devices: []marboragent.GPUInfo{
		{Index: 0, TemperatureC: thermalF(60)}, {Index: 1, TemperatureC: thermalF(95)}, {Index: 2, TemperatureC: thermalF(70)}}}}
	r.applyAgentTelemetry(ns[1], mid)
	if d, _ := thermalDraining(ns[1]); !d {
		t.Error("hot middle card must count")
	}
	// Non-contiguous indices: scope is matched on Index, not slice position.
	gap := marboragent.Telemetry{GPU: &marboragent.GPUBlock{Devices: []marboragent.GPUInfo{
		{Index: 2, TemperatureC: thermalF(95)}, {Index: 5, TemperatureC: thermalF(60)}}}}
	ns[2].DeclaredGPUIndices = []int{5}
	r.applyAgentTelemetry(ns[2], gap)
	if d, _ := thermalDraining(ns[2]); d {
		t.Error("scope [5] is the cool card; the hot card with Index 2 is out of scope")
	}
}

func TestThermalWatchdog_AgentBoundaryAtThresholdCounts(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(84.9)))
	if d, _ := thermalDraining(ns[0]); d {
		t.Error("below the threshold must not drain")
	}
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(85)))
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("exactly at the threshold counts (at or above)")
	}
}

func TestThermalWatchdog_AgentClearKeepsLocalStreakEvenWithAgentPresent(t *testing.T) {
	_, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	n.mu.Lock()
	n.VRAMSource = "nvidia" // local nvidia-smi is the source, agent also present
	n.AgentPresent = true
	n.ThermalBreaches = 2
	n.mu.Unlock()
	clearAgentTelemetry(n)
	if b := thermalBreachCount(n); b != 2 {
		t.Errorf("streak = %d, want 2: the agent was not the temperature source", b)
	}
}

func TestThermalWatchdog_UnreachableAgentPastThresholdResetsStreak(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	r.applyAgentTelemetry(n, thermalTel(thermalF(90)))
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.agentUnreachable(n)
	}
	if b := thermalBreachCount(n); b != 0 {
		t.Errorf("streak = %d after the agent went dark past the threshold, want 0", b)
	}
	if d, _ := thermalDraining(n); d {
		t.Error("an unreachable agent must not drain")
	}
}

func TestThermalWatchdog_AgentScopeVanishingTickFallsBackToHostMax(t *testing.T) {
	// Documents the stated rule: when a node's detected scope is absent for a
	// tick, no scope is known, so the host max counts.
	r, ns := agentThermalRouter(t, 1, "a")
	n := ns[0]
	tel := thermalTel(thermalF(60), thermalF(95))
	tel.Deployments = []marboragent.DeploymentReport{{Port: portOfOrDefault(n.URL), GPUGroup: []int{0},
		GPUScope: &marboragent.GPUScope{Indices: []int{0}, CrossChecked: true}}}
	r.applyAgentTelemetry(n, tel)
	if d, _ := thermalDraining(n); d {
		t.Fatal("verified scope [0] is cool")
	}
	tel.Deployments = nil
	r.applyAgentTelemetry(n, tel)
	if d, _ := thermalDraining(n); !d {
		t.Error("with the deployment report gone no scope is known: host max applies")
	}
}

func TestThermalWatchdog_AgentPersistFailureStillDrainsInMemory(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	st := openWarmTestStore(t)
	r.SetStore(st)
	st.Close() // every write now fails
	buf := captureLog(t)
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(95)))
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("in-memory drain must stand even when persisting fails")
	}
	if !strings.Contains(buf.String(), "persist drain for a") {
		t.Errorf("persist failure not logged: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "node a auto-drained after 1 consecutive polls at/above 85.0°C (agent GPU 0 at 95.0°C") {
		t.Errorf("drain not reported with its streak, threshold and reading: %q", buf.String())
	}
}

func TestThermalWatchdog_RemovedNodeIsNotPersistedOrLogged(t *testing.T) {
	r, _ := agentThermalRouter(t, 1, "a")
	st := openWarmTestStore(t)
	r.SetStore(st)
	buf := captureLog(t)
	r.thermalDrain("ghost", "agent GPU 0 at 99.0°C")
	if states, _ := st.NodeDrainStates(); len(states) != 0 {
		t.Errorf("a drain row was written for an unknown node: %+v", states)
	}
	if strings.Contains(buf.String(), "auto-drained") {
		t.Error("must not report a drain that did not happen")
	}
}

func TestThermalWatchdog_AgentLogsDeviceIndexNotPosition(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	buf := captureLog(t)
	tel := marboragent.Telemetry{GPU: &marboragent.GPUBlock{Devices: []marboragent.GPUInfo{
		{Index: 3, TemperatureC: thermalF(96)}, {Index: 6, TemperatureC: thermalF(50)}}}}
	r.applyAgentTelemetry(ns[0], tel)
	if !strings.Contains(buf.String(), "agent GPU 3 at 96.0°C") {
		t.Errorf("log %q should carry the device Index, not its position", buf.String())
	}
}

func TestThermalWatchdog_BreachLimitNeverBelowOne(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	r.thermalWatchdog.ConsecutiveBreaches = -1 // below one behaves as one (a struct-literal router can have 0)
	buf := captureLog(t)
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(90)))
	if d, _ := thermalDraining(ns[0]); !d {
		t.Error("a limit of 0 behaves as 1: first hot poll drains")
	}
	if !strings.Contains(buf.String(), "after 1 consecutive polls") {
		t.Errorf("log %q should report a limit of 1", buf.String())
	}
}

func TestThermalWatchdog_LocalPathDrainLogsReadingAndHonoursLimitFloor(t *testing.T) {
	r, _ := newThermalTestRouter(t, config.ThermalWatchdogConfig{
		Enabled: true, MaxTempCelsius: 85, ConsecutiveBreaches: 3,
	})
	r.thermalWatchdog.ConsecutiveBreaches = -2 // a limit below one behaves as one
	n := r.nodes[0]
	r.nvidiaMu.Lock()
	r.nvidiaCache[0] = GPUStats{TempCelsius: 91, VRAMTotalMB: 24576, VRAMUsedMB: 1000}
	r.nvidiaMu.Unlock()
	buf := captureLog(t)
	r.pollNode(n)
	if d, _ := thermalDraining(n); !d {
		t.Fatal("first hot local poll drains when the limit is below one")
	}
	if !strings.Contains(buf.String(), "node gpu-0 auto-drained after 1 consecutive polls at/above 85.0\u00b0C (local GPU at 91.0\u00b0C)") {
		t.Errorf("unexpected local drain log: %q", buf.String())
	}
}

func TestThermalWatchdog_UnscopedLogSaysWholeHost(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	buf := captureLog(t)
	r.applyAgentTelemetry(ns[0], thermalTel(thermalF(90)))
	if !strings.Contains(buf.String(), "agent GPU 0 at 90.0\u00b0C, no GPU scope known, whole host") {
		t.Errorf("log %q should say the whole host was used", buf.String())
	}
}
