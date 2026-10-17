package router

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

func fptr(v float64) *float64 { return &v }

// agentThermalRouter builds a router whose nodes all sit on one remote host,
// with the thermal watchdog at 85C and the given consecutive-breach count.
func agentThermalRouter(t *testing.T, consecutive int, names ...string) (*Router, []*NodeState) {
	t.Helper()
	var cfgs []config.NodeConfig
	for i, name := range names {
		cfgs = append(cfgs, config.NodeConfig{Name: name, URL: fmt.Sprintf("http://10.9.9.9:%d", 11434+i), Runtime: "ollama"})
	}
	r := New(config.RoutingConfig{
		Strategy:        "warm-first",
		ThermalWatchdog: config.ThermalWatchdogConfig{Enabled: true, MaxTempCelsius: 85, ConsecutiveBreaches: consecutive},
	}, cfgs, nil)
	return r, r.nodes
}

func gpuTel(temps ...*float64) marboragent.Telemetry {
	devs := make([]marboragent.GPUInfo, len(temps))
	for i, tc := range temps {
		devs[i] = marboragent.GPUInfo{Index: i, TemperatureC: tc, VRAMTotalMB: 24000}
	}
	return marboragent.Telemetry{GPU: &marboragent.GPUBlock{Count: len(devs), Devices: devs}}
}

func isDraining(n *NodeState) (bool, string) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Draining, n.DrainedReason
}

func breachCount(n *NodeState) int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ThermalBreaches
}

func TestThermalWatchdog_AgentTempDrainsAfterConsecutiveBreaches(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	for i := 1; i <= 2; i++ {
		r.applyAgentTelemetry(n, gpuTel(fptr(90)))
		if d, _ := isDraining(n); d {
			t.Fatalf("drained after only %d breach(es)", i)
		}
	}
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	if d, reason := isDraining(n); !d || reason != "thermal" {
		t.Fatalf("draining=%v reason=%q, want true/thermal", d, reason)
	}
}

func TestThermalWatchdog_AgentDrainPersistsAndLogsDevice(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	st := openWarmTestStore(t)
	r.SetStore(st)
	buf := captureLog(t)

	r.applyAgentTelemetry(ns[0], gpuTel(fptr(60), fptr(91.5)))

	states, err := st.NodeDrainStates()
	if err != nil {
		t.Fatal(err)
	}
	if !states["a"].Draining || states["a"].Reason != "thermal" {
		t.Errorf("persisted = %+v", states["a"])
	}
	if out := buf.String(); !strings.Contains(out, "GPU 1") || !strings.Contains(out, "91.5") {
		t.Errorf("log %q does not name the triggering device and reading", out)
	}
}

func TestThermalWatchdog_AgentUsesHottestInScopeOnly(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "hot", "cool")
	ns[0].DeclaredGPUIndices = []int{1}
	ns[1].DeclaredGPUIndices = []int{0}
	tel := gpuTel(fptr(60), fptr(95))
	for _, n := range ns {
		r.applyAgentTelemetryForHost(n, tel, 2)
	}
	if d, _ := isDraining(ns[0]); !d {
		t.Error("node scoped to the hot card should drain")
	}
	if d, _ := isDraining(ns[1]); d {
		t.Error("node scoped to the cool card must not drain")
	}
	ns[1].mu.RLock()
	temp := ns[1].Temperature
	ns[1].mu.RUnlock()
	if temp == nil || *temp != 60 {
		t.Errorf("display Temperature must stay device 0, got %v", temp)
	}
}

func TestThermalWatchdog_AgentUnscopedMultiMemberHostDrainsAll(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "x", "y")
	tel := gpuTel(fptr(60), fptr(95))
	for _, n := range ns {
		r.applyAgentTelemetryForHost(n, tel, 2)
	}
	for _, n := range ns {
		if d, _ := isDraining(n); !d {
			t.Errorf("%s: no scope known, host max applies", n.Name)
		}
	}
}

func TestThermalWatchdog_AgentVerifiedDetectedScope(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	n := ns[0]
	tel := gpuTel(fptr(60), fptr(95))
	tel.Deployments = []marboragent.DeploymentReport{{
		Port: portOfOrDefault(n.URL), GPUGroup: []int{0},
		GPUScope: &marboragent.GPUScope{Indices: []int{0}, CrossChecked: true},
	}}
	r.applyAgentTelemetry(n, tel)
	if d, _ := isDraining(n); d {
		t.Error("verified scope [0] is cool, must not drain on card 1")
	}
	tel.Deployments[0].GPUScope = &marboragent.GPUScope{Indices: []int{0}, CrossChecked: false}
	r.applyAgentTelemetry(n, tel)
	if d, _ := isDraining(n); !d {
		t.Error("unverified scope must fall back to host max and drain")
	}
}

func TestThermalWatchdog_AgentScopeMatchingNoDeviceUsesHostMax(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	ns[0].DeclaredGPUIndices = []int{7}
	r.applyAgentTelemetry(ns[0], gpuTel(fptr(95)))
	if d, _ := isDraining(ns[0]); !d {
		t.Error("scope matching no device falls back to host max")
	}
}

func TestThermalWatchdog_AgentPartialNilUsesMaxOfKnown(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	r.applyAgentTelemetry(ns[0], gpuTel(nil, fptr(90)))
	if d, _ := isDraining(ns[0]); !d {
		t.Error("max over devices that report a reading should drain")
	}
}

func TestThermalWatchdog_AgentScopedDevicesWithoutReadingNeverBorrow(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	ns[0].DeclaredGPUIndices = []int{0}
	r.applyAgentTelemetry(ns[0], gpuTel(nil, fptr(95)))
	if d, _ := isDraining(ns[0]); d {
		t.Error("in-scope card reports nothing: unknown, must not borrow a sibling reading")
	}
}

func TestThermalWatchdog_AgentUnknownResetsAndNeverDrains(t *testing.T) {
	cases := map[string]marboragent.Telemetry{
		"nil temp":      gpuTel(nil),
		"no devices":    gpuTel(),
		"no gpu block":  {},
		"all nil multi": gpuTel(nil, nil),
	}
	for name, unknown := range cases {
		t.Run(name, func(t *testing.T) {
			r, ns := agentThermalRouter(t, 3, "a")
			n := ns[0]
			r.applyAgentTelemetry(n, gpuTel(fptr(90)))
			r.applyAgentTelemetry(n, gpuTel(fptr(90)))
			r.applyAgentTelemetry(n, unknown)
			if b := breachCount(n); b != 0 {
				t.Errorf("breaches = %d, want 0 after unknown reading", b)
			}
			r.applyAgentTelemetry(n, gpuTel(fptr(90)))
			if d, _ := isDraining(n); d {
				t.Error("unknown reading must reset the streak")
			}
		})
	}
}

func TestThermalWatchdog_AgentCoolingResets(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.applyAgentTelemetry(n, gpuTel(fptr(70)))
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	if d, _ := isDraining(n); d {
		t.Error("a cool poll must reset the streak")
	}
}

func TestThermalWatchdog_AgentSkippedWhenLocalNvidia(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	n := ns[0]
	n.mu.Lock()
	n.VRAMSource = "nvidia"
	n.mu.Unlock()
	r.applyAgentTelemetry(n, gpuTel(fptr(99)))
	if d, _ := isDraining(n); d || breachCount(n) != 0 {
		t.Error("agent temperature must not count while the local nvidia-smi is the source")
	}
}

func TestThermalWatchdog_AgentDisabledNeverDrains(t *testing.T) {
	for _, cfg := range []config.ThermalWatchdogConfig{
		{Enabled: false, MaxTempCelsius: 85, ConsecutiveBreaches: 1},
		{Enabled: true, MaxTempCelsius: 0, ConsecutiveBreaches: 1},
	} {
		r := New(config.RoutingConfig{Strategy: "warm-first", ThermalWatchdog: cfg},
			[]config.NodeConfig{{Name: "a", URL: "http://10.9.9.9:11434", Runtime: "ollama"}}, nil)
		r.applyAgentTelemetry(r.nodes[0], gpuTel(fptr(99)))
		if d, _ := isDraining(r.nodes[0]); d || breachCount(r.nodes[0]) != 0 {
			t.Errorf("cfg %+v counted or drained", cfg)
		}
	}
}

func TestThermalWatchdog_AgentManualDrainNotOverridden(t *testing.T) {
	r, ns := agentThermalRouter(t, 1, "a")
	r.DrainNode("a", "maintenance", 0)
	buf := captureLog(t)
	r.applyAgentTelemetry(ns[0], gpuTel(fptr(99)))
	if _, reason := isDraining(ns[0]); reason != "maintenance" {
		t.Errorf("reason = %q, manual drain must stay", reason)
	}
	if strings.Contains(buf.String(), "thermal watchdog") {
		t.Error("no thermal drain log while already draining")
	}
}

func TestThermalWatchdog_AgentUndrainWhileHotRedrains(t *testing.T) {
	r, ns := agentThermalRouter(t, 2, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.UndrainNode("a")
	if b := breachCount(n); b < 2 {
		t.Errorf("breaches = %d, the drain must not reset the streak", b)
	}
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	if d, _ := isDraining(n); !d {
		t.Error("still-hot node must redrain on the next poll, like the local path")
	}
}

func TestThermalWatchdog_AgentFlapBelowFailureThresholdKeepsStreak(t *testing.T) {
	r, ns := agentThermalRouter(t, 2, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.agentUnreachable(n) // one dropped poll, below the failure threshold
	if b := breachCount(n); b != 1 {
		t.Fatalf("breaches = %d after one dropped poll, want 1", b)
	}
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	if d, _ := isDraining(n); !d {
		t.Error("streak must survive a single dropped poll")
	}
}

func TestThermalWatchdog_AgentGoneAndOversizeResetStreak(t *testing.T) {
	r, ns := agentThermalRouter(t, 3, "a")
	n := ns[0]
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	applyOversizeTelemetry(n)
	if b := breachCount(n); b != 0 {
		t.Errorf("oversize: breaches = %d, want 0", b)
	}
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	clearAgentTelemetry(n)
	if b := breachCount(n); b != 0 {
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
	if b := breachCount(n); b != 2 {
		t.Errorf("local streak = %d, want 2 untouched", b)
	}
	// Agent-less host whose local read just failed (VRAMSource moved off nvidia).
	n.mu.Lock()
	n.VRAMSource = "none"
	n.AgentPresent = false
	n.mu.Unlock()
	clearAgentTelemetry(n)
	if b := breachCount(n); b != 2 {
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
	r.applyAgentTelemetry(n, gpuTel(fptr(90)))
	if d, _ := isDraining(n); !d {
		t.Error("a hot agent reading continues the same node's streak")
	}
}

func TestThermalWatchdog_LocalAndAgentConcurrentRace(t *testing.T) {
	r, ns := agentThermalRouter(t, 1000, "a")
	n := ns[0]
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.applyAgentTelemetry(n, gpuTel(fptr(90)))
				applyOversizeTelemetry(n)
				clearAgentTelemetry(n)
			}
		}()
	}
	wg.Wait()
}
