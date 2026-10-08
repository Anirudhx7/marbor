package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
)

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }

// psModelsServer answers /api/ps like Ollama with one resident model.
func psModelsServer(t *testing.T, sizeVRAM int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": "m", "size_vram": sizeVRAM}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// agentTestRouter returns a router with one node "gpu-0" at a loopback URL and
// an enabled agent at the given port.
func agentTestRouter(t *testing.T, agentPort int) (*Router, *NodeState) {
	t.Helper()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: "http://127.0.0.1:11434"}}, nil)
	n := r.nodes[0]
	r.SetMarborAgent(n.Host, true, agentPort, "secret-token-123", "http")
	return r, n
}

func agentServerPort(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return mustPort(t, srv.URL)
}

// --- pollNode: telemetry that other sources own ---

func TestPollNodeKeepsAgentTemperatureAndPower(t *testing.T) {
	srv := psModelsServer(t, 0)
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: srv.URL, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.AgentPresent = true
	n.Temperature = floatPtr(61)
	n.PowerDrawW = 205
	n.VRAMSource = "agent"
	n.VRAMTotalMB = 24000
	n.VRAMUsedMB = 100
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.Temperature == nil || *n.Temperature != 61 {
		t.Errorf("Temperature = %v, want the agent reading kept", n.Temperature)
	}
	if n.PowerDrawW != 205 {
		t.Errorf("PowerDrawW = %v, want the agent reading kept", n.PowerDrawW)
	}
}

func TestPollNodeClearsTemperatureAndPowerWithoutAgent(t *testing.T) {
	srv := psModelsServer(t, 0)
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: srv.URL, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Temperature = floatPtr(61)
	n.PowerDrawW = 205
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.Temperature != nil || n.PowerDrawW != 0 {
		t.Errorf("Temperature=%v PowerDrawW=%v, want both unknown when no agent supplies them", n.Temperature, n.PowerDrawW)
	}
}

func TestPollNodeAgentVRAMTotalKeptWhenRuntimeReportsUsed(t *testing.T) {
	srv := psModelsServer(t, 2<<30)
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: srv.URL, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.AgentPresent = true
	n.VRAMSource = "agent"
	n.VRAMTotalMB = 24000
	n.VRAMUsedMB = 100
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMUsedMB != 2048 {
		t.Errorf("VRAMUsedMB = %d, want the runtime's own 2048", n.VRAMUsedMB)
	}
	if n.VRAMTotalMB != 24000 || n.VRAMSource != "agent" {
		t.Errorf("total/source = %d/%q, want the agent's 24000/agent kept (only the used figure is the runtime's)", n.VRAMTotalMB, n.VRAMSource)
	}
}

// --- applyAgentTelemetry ---

func gpuTelemetry(dev marboragent.GPUInfo) marboragent.Telemetry {
	return marboragent.Telemetry{GPU: &marboragent.GPUBlock{Vendor: "nvidia", Count: 1, Devices: []marboragent.GPUInfo{dev}}}
}

func TestApplyAgentTelemetryMissingPowerReadsAsUnknown(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.PowerDrawW = 200
	n.mu.Unlock()

	r.applyAgentTelemetry(n, gpuTelemetry(marboragent.GPUInfo{TemperatureC: floatPtr(50), VRAMTotalMB: 100, VRAMUsedMB: 10}))

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.PowerDrawW != 0 {
		t.Fatalf("PowerDrawW = %v, want 0 when the agent omits power (a stale reading must not stay current)", n.PowerDrawW)
	}
}

func TestApplyAgentTelemetryNoGPUDropsAgentVRAM(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.VRAMSource = "agent"
	n.VRAMTotalMB = 24000
	n.VRAMUsedMB = 5000
	n.Temperature = floatPtr(60)
	n.PowerDrawW = 150
	n.mu.Unlock()

	r.applyAgentTelemetry(n, marboragent.Telemetry{})

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource == "agent" || n.VRAMTotalMB != 0 || n.VRAMUsedMB != 0 {
		t.Errorf("VRAM = %d/%d source %q, want agent VRAM dropped once the agent reports no GPU", n.VRAMUsedMB, n.VRAMTotalMB, n.VRAMSource)
	}
	if n.Temperature != nil || n.PowerDrawW != 0 {
		t.Errorf("Temperature=%v PowerDrawW=%v, want cleared with the GPU gone", n.Temperature, n.PowerDrawW)
	}
}

func TestApplyAgentTelemetryZeroDeviceVRAMDropsAgentVRAM(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.VRAMSource = "agent"
	n.VRAMTotalMB = 24000
	n.VRAMUsedMB = 5000
	n.mu.Unlock()

	r.applyAgentTelemetry(n, gpuTelemetry(marboragent.GPUInfo{TemperatureC: floatPtr(50)}))

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource == "agent" || n.VRAMTotalMB != 0 || n.VRAMUsedMB != 0 {
		t.Errorf("VRAM = %d/%d source %q, want the stale agent VRAM dropped when the device reports 0/0", n.VRAMUsedMB, n.VRAMTotalMB, n.VRAMSource)
	}
}

func TestApplyAgentTelemetryMissingHostClearsCPU(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.CPUPercent = 55
	n.mu.Unlock()

	r.applyAgentTelemetry(n, marboragent.Telemetry{})

	n.mu.RLock()
	cpu := n.CPUPercent
	n.mu.RUnlock()
	if cpu != 0 {
		t.Fatalf("CPUPercent = %v, want 0 when the report has no host block", cpu)
	}
}

func TestApplyAgentTelemetryHostWithoutCPUKeepsPreviousValue(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.CPUPercent = 55
	n.mu.Unlock()

	r.applyAgentTelemetry(n, marboragent.Telemetry{Host: &marboragent.HostTelemetry{}})

	n.mu.RLock()
	cpu := n.CPUPercent
	n.mu.RUnlock()
	if cpu != 55 {
		t.Fatalf("CPUPercent = %v, want the earlier value kept when the host block omits CPU", cpu)
	}
}

// --- clearAgentTelemetry / TLS flag ---

func TestClearAgentTelemetryClearsControlDiscovery(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.AgentControlDiscoveredDriver = "systemd"
	n.AgentControlDiscoveredIdentifier = "ollama.service"
	n.AgentControlDiscoveredEvidence = []string{"unit found"}
	n.mu.Unlock()

	clearAgentTelemetry(n)

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentControlDiscoveredDriver != "" || n.AgentControlDiscoveredIdentifier != "" || n.AgentControlDiscoveredEvidence != nil {
		t.Fatalf("control discovery left behind: %q %q %v", n.AgentControlDiscoveredDriver, n.AgentControlDiscoveredIdentifier, n.AgentControlDiscoveredEvidence)
	}
}

func TestPollAgentHostsNoAgentResetsTLSMismatch(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.AgentTLSMismatch = true
	n.mu.Unlock()

	r.pollAgentHosts() // no agent configured for the host

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentTLSMismatch {
		t.Fatal("AgentTLSMismatch must reset when no agent is configured any more")
	}
}

func TestPollAgentHostSetupErrorsResetTLSMismatch(t *testing.T) {
	cfg := MarborAgentConfig{Enabled: true, Port: 9200, Scheme: "http"}
	for _, host := range []string{"", "bad host"} {
		t.Run(fmt.Sprintf("host=%q", host), func(t *testing.T) {
			r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
			n := r.nodes[0]
			n.mu.Lock()
			n.AgentTLSMismatch = true
			n.mu.Unlock()

			r.pollAgentHost(host, cfg, []*NodeState{n})

			n.mu.RLock()
			defer n.mu.RUnlock()
			if n.AgentTLSMismatch {
				t.Fatal("AgentTLSMismatch must reset when the poll never reached the network")
			}
			if n.AgentFailures != 1 {
				t.Fatalf("AgentFailures = %d, want 1", n.AgentFailures)
			}
		})
	}
}

// --- agent up/down transitions ---

func TestAgentUnreachableNeverReachedLeavesNoTransitionState(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.agentUnreachable(n)
	}
	r.mu.RLock()
	_, seen := r.prevAgentPresent["n"]
	r.mu.RUnlock()
	if seen {
		t.Fatal("an agent that was never reached must not be recorded as having gone down, or its first success fires a spurious agent_up")
	}
}

func TestAgentUnreachableAfterReachableRecordsDown(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	r.agentReachable("n")
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.agentUnreachable(n)
	}
	r.mu.RLock()
	prev, seen := r.prevAgentPresent["n"]
	r.mu.RUnlock()
	if !seen || prev {
		t.Fatalf("prevAgentPresent = %v (seen %v), want recorded as down after a real outage", prev, seen)
	}
}

// --- deployment / runtime matching ---

func TestPortOf(t *testing.T) {
	cases := map[string]int{
		"http://host:8080":  8080,
		"https://host:8443": 8443,
		"http://host":       80,
		"https://host":      443,
		"http://[::1]:9000": 9000,
		"ftp://host":        0,
		"http://host:abc":   0,
		"::not a url":       0,
	}
	for in, want := range cases {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMatchRuntimeTiers(t *testing.T) {
	tel := marboragent.Telemetry{Runtimes: []marboragent.RuntimeInfo{
		{ID: "a", Name: "ollama", Port: 11434},
		{ID: "b", Name: "vllm", Port: 8000},
	}}
	if e, id := matchRuntime(tel, "b", 11434); e == nil || e.ID != "b" || id != "b" {
		t.Errorf("a pinned id must win over the port: got %+v %q", e, id)
	}
	if e, id := matchRuntime(tel, "gone", 8000); e == nil || e.ID != "b" || id != "b" {
		t.Errorf("a pin no longer reported must fall back to the port and re-pin: got %+v %q", e, id)
	}
	if e, id := matchRuntime(tel, "", 11434); e == nil || e.ID != "a" || id != "a" {
		t.Errorf("first contact must bootstrap by port: got %+v %q", e, id)
	}
	if e, _ := matchRuntime(tel, "", 9999); e != nil {
		t.Errorf("no pin and no port match must be nil, got %+v", e)
	}
	legacy := marboragent.Telemetry{Runtime: &marboragent.RuntimeInfo{Name: "ollama"}}
	if e, id := matchRuntime(legacy, "pinned", 1); e == nil || e.Name != "ollama" || id != "pinned" {
		t.Errorf("an old agent with only the singular runtime must be used with the pin unchanged: got %+v %q", e, id)
	}
	// The legacy field is only a fallback when the list is empty.
	mixed := marboragent.Telemetry{Runtime: &marboragent.RuntimeInfo{Name: "legacy"}, Runtimes: tel.Runtimes}
	if e, _ := matchRuntime(mixed, "", 9999); e != nil {
		t.Errorf("a non-empty runtimes list must not fall back to the legacy field, got %+v", e)
	}
}

func deploymentAgentTelemetry() marboragent.Telemetry {
	return marboragent.Telemetry{
		Deployments: []marboragent.DeploymentReport{
			{Runtime: "vllm", Port: 0, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 4}},
		},
	}
}

func TestPortlessDeploymentNotAttributedOnMultiNodeHost(t *testing.T) {
	agentPort := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(deploymentAgentTelemetry())
	})
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "ollama", URL: "http://127.0.0.1:11434"},
		{Name: "vllm", URL: "http://127.0.0.1:8000"},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	r.pollAgentHosts()

	for _, n := range r.nodes {
		n.mu.RLock()
		got := n.DetectedRuntime
		n.mu.RUnlock()
		if got != "" {
			t.Errorf("node %s got deployment %q: a report with no port cannot be attributed to one of several nodes on a host", n.Name, got)
		}
	}
}

func TestPortlessDeploymentAttributedOnSingleNodeHost(t *testing.T) {
	agentPort := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(deploymentAgentTelemetry())
	})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "only", URL: "http://127.0.0.1:11434"}}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	r.pollAgentHosts()

	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.DetectedRuntime != "vllm" || n.DetectedParallelismWidth != 4 {
		t.Fatalf("detected %q width %d, want the sole node to take the portless report", n.DetectedRuntime, n.DetectedParallelismWidth)
	}
}

// --- buildAgentURL ---

func TestBuildAgentURL(t *testing.T) {
	cases := []struct {
		name, host, scheme, want string
		port                     int
		wantErr                  bool
	}{
		{name: "plain host", host: "gpu-1", port: 9200, want: "http://gpu-1:9200/v1/status"},
		{name: "https", host: "gpu-1", scheme: "https", port: 9200, want: "https://gpu-1:9200/v1/status"},
		{name: "host with scheme and port is stripped", host: "http://gpu-1:11434", port: 9200, want: "http://gpu-1:9200/v1/status"},
		{name: "bare ipv6", host: "2001:db8::10", port: 9200, want: "http://[2001:db8::10]:9200/v1/status"},
		{name: "bracketed ipv6 url", host: "http://[::1]:11434", port: 9200, want: "http://[::1]:9200/v1/status"},
		{name: "empty", host: "", port: 9200, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildAgentURL(c.host, c.port, c.scheme)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// --- health hysteresis ---

func TestHealthSuccessThresholdHysteresis(t *testing.T) {
	srv := psModelsServer(t, 0)
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: srv.URL, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	if r.healthSuccessThreshold != 2 {
		t.Fatalf("default success threshold = %d, want 2", r.healthSuccessThreshold)
	}
	n.mu.Lock()
	n.Healthy = false
	n.mu.Unlock()

	healthy := func() bool {
		n.mu.RLock()
		defer n.mu.RUnlock()
		return n.Healthy
	}
	r.pollNode(n)
	if healthy() {
		t.Fatal("one success after an outage must not restore the node")
	}
	r.markFailure(n) // a failure resets the run of successes
	r.pollNode(n)
	if healthy() {
		t.Fatal("successes must be consecutive: success, failure, success is still not enough")
	}
	r.pollNode(n)
	if !healthy() {
		t.Fatal("two consecutive successes must restore the node")
	}
}

// --- ProbeNodeOnDemand ---

func TestProbeNodeOnDemand(t *testing.T) {
	srv := psModelsServer(t, 0)
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: srv.URL, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	ctx := context.Background()

	if _, _, _, found := r.ProbeNodeOnDemand(ctx, "ghost"); found {
		t.Error("an unknown node must report found=false")
	}

	n.mu.Lock()
	n.probe = nil
	n.mu.Unlock()
	if ok, msg, _, found := r.ProbeNodeOnDemand(ctx, "gpu-0"); ok || !found || !strings.Contains(msg, "auto-detecting") {
		t.Errorf("nil probe: ok=%v found=%v msg=%q, want a not-yet-detected error", ok, found, msg)
	}

	n.mu.Lock()
	n.probe = fixedErrProbe{err: errors.New("connection refused")}
	n.mu.Unlock()
	if ok, msg, _, found := r.ProbeNodeOnDemand(ctx, "gpu-0"); ok || !found || msg != "connection refused" {
		t.Errorf("failing probe: ok=%v found=%v msg=%q", ok, found, msg)
	}

	n.mu.Lock()
	n.probe = runtimepkg.NewProbe("ollama", http.DefaultClient)
	n.Failures = 2
	n.Healthy = false
	n.ConsecutiveSuccesses = 1
	before := n.LastPollAt
	n.mu.Unlock()
	if ok, msg, _, found := r.ProbeNodeOnDemand(ctx, "gpu-0"); !ok || !found || msg != "" {
		t.Errorf("healthy probe: ok=%v found=%v msg=%q", ok, found, msg)
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.Failures != 2 || n.Healthy || n.ConsecutiveSuccesses != 1 || !n.LastPollAt.Equal(before) {
		t.Errorf("an on-demand probe changed poller state: failures=%d healthy=%v successes=%d", n.Failures, n.Healthy, n.ConsecutiveSuccesses)
	}
}

// --- FleetModelInventory ---

func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFleetModelInventoryFetchErrorIsIncompleteButKeepsOthers(t *testing.T) {
	good, _ := tagsServer(t, map[string]int64{"m-good": 1})
	bad := statusServer(t, http.StatusInternalServerError)
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "good", URL: good.URL, Runtime: "ollama"},
		{Name: "broken-node", URL: bad.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = true
		n.mu.Unlock()
	}
	logs := captureLog(t)

	seen, complete := r.FleetModelInventory()
	if complete {
		t.Error("inventory must be incomplete when a healthy node's catalog could not be read")
	}
	if seen["m-good"] != FleetModelAvailable {
		t.Errorf("seen = %v, want the reachable node's models kept", seen)
	}
	if !strings.Contains(logs.String(), "broken-node") {
		t.Errorf("the failing node must be named in the log, got %q", logs.String())
	}
}

func TestFleetModelInventoryNoHealthyNodeIsIncomplete(t *testing.T) {
	srv, hits := tagsServer(t, map[string]int64{"m": 1})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false
	r.nodes[0].mu.Unlock()

	seen, complete := r.FleetModelInventory()
	if complete || len(seen) != 0 {
		t.Fatalf("seen=%v complete=%v, want empty and incomplete", seen, complete)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Error("an unhealthy node must not be queried")
	}
}

func TestFleetModelInventoryLoadedWinsOverAvailable(t *testing.T) {
	for _, loadedFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("loadedNodeFirst=%v", loadedFirst), func(t *testing.T) {
			srvA, _ := tagsServer(t, map[string]int64{"m1": 1, "m2": 1})
			srvB, _ := tagsServer(t, map[string]int64{"m1": 1})
			cfgs := []config.NodeConfig{
				{Name: "loaded", URL: srvA.URL, Runtime: "ollama"},
				{Name: "other", URL: srvB.URL, Runtime: "ollama"},
			}
			if !loadedFirst {
				cfgs[0], cfgs[1] = cfgs[1], cfgs[0]
			}
			r := New(config.RoutingConfig{}, cfgs, nil)
			for _, n := range r.nodes {
				n.mu.Lock()
				n.Healthy = true
				if n.Name == "loaded" {
					n.LoadedModels = []ModelInfo{{Name: "m1"}}
				}
				n.mu.Unlock()
			}
			seen, complete := r.FleetModelInventory()
			if !complete {
				t.Fatal("want a complete inventory")
			}
			if seen["m1"] != FleetModelLoaded || seen["m2"] != FleetModelAvailable {
				t.Fatalf("seen = %v, want m1 loaded (wins over available) and m2 available", seen)
			}
		})
	}
}

// --- host evidence deep copy ---

func TestRecordHostEvidenceDeepCopiesDeployments(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	nodes := 2
	rank := 0
	port := 29500
	dep := marboragent.DeploymentReport{
		Runtime:     "vllm",
		Port:        8000,
		GPUGroup:    []int{0, 1},
		GPUScope:    &marboragent.GPUScope{Indices: []int{0, 1}, UUIDs: []string{"u0", "u1"}},
		Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 2},
		Caps:        &marboragent.RuntimeCaps{TP: true},
		Topology: &marboragent.Topology{
			NNodes: &nodes, NodeRank: &rank, MasterPort: &port,
			RPCServers: []string{"a:1"}, Evidence: []string{"e1"},
		},
	}
	ev := HostEvidence{Host: "h", Addrs: []string{"10.0.0.1"}, Capabilities: []string{"status"}, Deployments: []marboragent.DeploymentReport{dep}}
	r.RecordHostEvidence(ev)

	// Mutate everything the caller still holds.
	ev.Addrs[0] = "mutated"
	ev.Capabilities[0] = "mutated"
	ev.Deployments[0].GPUGroup[0] = 99
	ev.Deployments[0].GPUScope.Indices[0] = 99
	ev.Deployments[0].GPUScope.UUIDs[0] = "mutated"
	ev.Deployments[0].Parallelism.Width = 99
	ev.Deployments[0].Caps.TP = false
	*ev.Deployments[0].Topology.NNodes = 99
	*ev.Deployments[0].Topology.NodeRank = 99
	*ev.Deployments[0].Topology.MasterPort = 99
	ev.Deployments[0].Topology.RPCServers[0] = "mutated"
	ev.Deployments[0].Topology.Evidence[0] = "mutated"

	got := r.snapshotHostEvidence()["h"]
	d := got.Deployments[0]
	if got.Addrs[0] != "10.0.0.1" || got.Capabilities[0] != "status" {
		t.Errorf("addrs/capabilities aliased the caller's slices: %v %v", got.Addrs, got.Capabilities)
	}
	if d.GPUGroup[0] != 0 || d.GPUScope.Indices[0] != 0 || d.GPUScope.UUIDs[0] != "u0" {
		t.Errorf("GPU group/scope aliased: %v %+v", d.GPUGroup, d.GPUScope)
	}
	if d.Parallelism.Width != 2 || !d.Caps.TP {
		t.Errorf("parallelism/caps aliased: %+v %+v", d.Parallelism, d.Caps)
	}
	tp := d.Topology
	if *tp.NNodes != 2 || *tp.NodeRank != 0 || *tp.MasterPort != 29500 || tp.RPCServers[0] != "a:1" || tp.Evidence[0] != "e1" {
		t.Errorf("topology aliased: nnodes=%d rank=%d port=%d rpc=%v ev=%v", *tp.NNodes, *tp.NodeRank, *tp.MasterPort, tp.RPCServers, tp.Evidence)
	}
}

func TestCopyDeploymentsNil(t *testing.T) {
	if copyDeployments(nil) != nil {
		t.Error("nil in must give nil out")
	}
	if copyTopology(nil) != nil {
		t.Error("nil topology must stay nil")
	}
}

// --- local interface lookup ---

func TestLocalInterfaceAddrsKeepsPreviousSetWhenLookupFails(t *testing.T) {
	prevFn := interfaceAddrs
	t.Cleanup(func() {
		interfaceAddrs = prevFn
		localAddrMu.Lock()
		localAddrCache = nil
		localAddrAt = time.Time{}
		localAddrMu.Unlock()
	})

	localAddrMu.Lock()
	localAddrCache = map[string]struct{}{"192.0.2.7": {}}
	localAddrAt = time.Now().Add(-2 * localAddrCacheTTL) // stale, forces a refresh
	localAddrMu.Unlock()
	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("simulated failure") }

	got := localInterfaceAddrs()
	if _, ok := got["192.0.2.7"]; !ok {
		t.Fatalf("a failed lookup must keep the previous set, got %v", got)
	}
	if !isLocalNode("http://192.0.2.7:11434") {
		t.Error("a node on a previously known local address must still read as local")
	}
}

func TestLocalInterfaceAddrsDoesNotCacheEmptySetOnFirstFailure(t *testing.T) {
	prevFn := interfaceAddrs
	t.Cleanup(func() {
		interfaceAddrs = prevFn
		localAddrMu.Lock()
		localAddrCache = nil
		localAddrAt = time.Time{}
		localAddrMu.Unlock()
	})
	localAddrMu.Lock()
	localAddrCache = nil
	localAddrAt = time.Time{}
	localAddrMu.Unlock()

	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("simulated failure") }
	_ = localInterfaceAddrs()

	localAddrMu.RLock()
	cached := localAddrCache
	localAddrMu.RUnlock()
	if cached != nil {
		t.Fatalf("an empty set from a failed lookup was cached: %v", cached)
	}
}

// --- typed /health 404 detection ---

func TestPollNodeMLXHintUsesTypedHealthError(t *testing.T) {
	newNode := func(err error) (*Router, *NodeState) {
		r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:8080", Runtime: "llamacpp"}}, nil)
		n := r.nodes[0]
		n.mu.Lock()
		n.probe = fixedErrProbe{err: err}
		n.mu.Unlock()
		return r, n
	}
	hint := func(n *NodeState) string {
		n.mu.RLock()
		defer n.mu.RUnlock()
		return n.RuntimeMismatchHint
	}

	r, n := newNode(fmt.Errorf("llamacpp probe: %w", &runtimepkg.HealthStatusError{StatusCode: 404}))
	r.pollNode(n)
	if hint(n) == "" {
		t.Error("a typed /health 404 on a llamacpp node must set the hint")
	}

	r, n = newNode(fmt.Errorf("llamacpp probe: %w", &runtimepkg.HealthStatusError{StatusCode: 503}))
	r.pollNode(n)
	if hint(n) != "" {
		t.Error("a /health 503 must not set the 404 hint")
	}

	r, n = newNode(errors.New("proxy said: /health returned 404 for another reason"))
	r.pollNode(n)
	if hint(n) != "" {
		t.Error("error text alone must not trigger the hint; only the typed error does")
	}
}

// --- rate-limited failure logging ---

func countOf(s, sub string) int { return strings.Count(s, sub) }

func TestAgentPollFailuresAreLoggedOnceWithoutSecrets(t *testing.T) {
	type tc struct {
		name string
		want string
		run  func(t *testing.T, r *Router, n *NodeState)
	}
	cases := []tc{
		{"non-200 status", "answered status 500", func(t *testing.T, r *Router, n *NodeState) {
			port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(500) })
			r.SetMarborAgent(n.Host, true, port, "secret-token-123", "http")
			r.pollAgentHosts()
			r.pollAgentHosts()
		}},
		{"undecodable reply", "could not decode", func(t *testing.T, r *Router, n *NodeState) {
			port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("not json")) })
			r.SetMarborAgent(n.Host, true, port, "secret-token-123", "http")
			r.pollAgentHosts()
			r.pollAgentHosts()
		}},
		{"dial failure", "agent poll for host", func(t *testing.T, r *Router, n *NodeState) {
			srv := httptest.NewServer(http.NotFoundHandler())
			port := mustPort(t, srv.URL)
			srv.Close()
			r.SetMarborAgent(n.Host, true, port, "secret-token-123", "http")
			r.pollAgentHosts()
			r.pollAgentHosts()
		}},
		{"tls fingerprint mismatch", "TLS fingerprint mismatch", func(t *testing.T, r *Router, n *NodeState) {
			r.client = &http.Client{Transport: auditRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("dial: %w", ErrTLSFingerprintMismatch)
			})}
			r.SetMarborAgent(n.Host, true, 9200, "secret-token-123", "https")
			r.pollAgentHosts()
			r.pollAgentHosts()
		}},
		{"invalid agent address", "invalid agent address", func(t *testing.T, r *Router, n *NodeState) {
			cfg := MarborAgentConfig{Enabled: true, Port: 9200, Token: "secret-token-123"}
			r.pollAgentHost("", cfg, []*NodeState{n})
			r.pollAgentHost("", cfg, []*NodeState{n})
		}},
		{"unbuildable request", "cannot build request", func(t *testing.T, r *Router, n *NodeState) {
			cfg := MarborAgentConfig{Enabled: true, Port: 9200, Token: "secret-token-123"}
			r.pollAgentHost("bad host", cfg, []*NodeState{n})
			r.pollAgentHost("bad host", cfg, []*NodeState{n})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: "http://127.0.0.1:11434"}}, nil)
			logs := captureLog(t)
			c.run(t, r, r.nodes[0])
			out := logs.String()
			if got := countOf(out, c.want); got != 1 {
				t.Errorf("%q logged %d times, want exactly once across two polls:\n%s", c.want, got, out)
			}
			if strings.Contains(out, "secret-token-123") {
				t.Errorf("the agent token leaked into the log:\n%s", out)
			}
		})
	}
}

type auditRoundTripper func(*http.Request) (*http.Response, error)

func (f auditRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestProbeFailureIsLoggedOnce(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: "http://example.test:11434", Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.probe = fixedErrProbe{err: errors.New("connection refused by test")}
	n.mu.Unlock()
	logs := captureLog(t)

	r.pollNode(n)
	r.pollNode(n)

	out := logs.String()
	if countOf(out, "connection refused by test") != 1 || !strings.Contains(out, "gpu-0") {
		t.Fatalf("want the probe failure logged once and naming the node, got:\n%s", out)
	}
}

func TestDockerDiscoveryErrorIsLoggedOnce(t *testing.T) {
	socket := "/nonexistent/docker.sock"
	if runtime.GOOS == "windows" {
		socket = "tcp://127.0.0.1:1"
	}
	r := New(config.RoutingConfig{}, nil, nil)
	r.dockerCfg = config.DockerConfig{Enabled: true, Socket: socket}
	logs := captureLog(t)

	r.discoverAndAddDockerNodes()
	r.discoverAndAddDockerNodes()

	if got := countOf(logs.String(), "docker discovery failed"); got != 1 {
		t.Fatalf("docker discovery failure logged %d times, want once:\n%s", got, logs.String())
	}
}
