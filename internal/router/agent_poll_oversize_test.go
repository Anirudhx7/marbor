package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// agentMode selects what the switchable test agent answers with.
type agentMode int32

const (
	modeGood agentMode = iota
	modeOversize
	modeDrop // 503, a plain failed poll
	modeReadError
)

// richTelemetry is a status report that populates every reading the oversize
// path is expected to forget and every identity field it must keep.
func richTelemetry() marboragent.Telemetry {
	temp, fan, power, cpu := 71.0, 55.0, 280.0, 33.0
	running, waiting, kv := 2, 1, 40.0
	return marboragent.Telemetry{
		Agent:        marboragent.Agent{NodeID: "id-1", Version: "v0.24.0", ProtocolVersion: 1, Platform: "linux", Architecture: "amd64"},
		Capabilities: []string{"status", "runtime.control"},
		GPU: &marboragent.GPUBlock{
			Count: 1, Vendor: "nvidia", DriverVersion: "550", CUDAVersion: "12.4",
			Devices: []marboragent.GPUInfo{{Index: 0, Vendor: "nvidia", TemperatureC: &temp, FanPercent: &fan, PowerWatts: &power, VRAMUsedMB: 8000, VRAMTotalMB: 16000}},
		},
		Host: &marboragent.HostTelemetry{
			CPUPercent: &cpu, RAMUsedMB: 4000, RAMTotalMB: 32000, DiskFreeGB: 100, DiskTotalGB: 500,
			Hostname: "gpu-host", UptimeSeconds: 9000, BootTime: 1700000000, Addrs: []string{"10.0.0.5"},
		},
		Runtimes: []marboragent.RuntimeInfo{{
			ID: "rt-1", Name: "vllm", Version: "0.6", Status: "running", Port: 0,
			Engine: &marboragent.EngineState{RunningRequests: &running, WaitingRequests: &waiting, KVCacheUsagePercent: &kv},
		}},
		Deployments: []marboragent.DeploymentReport{{
			Runtime: "vllm", RuntimeID: "rt-1", GPUGroup: []int{0},
			Parallelism: &marboragent.ParallelismInfo{Type: "tensor", Width: 2},
			Source:      "ps",
		}},
	}
}

// switchAgent serves richTelemetry, an oversize padded body, a 503, or a body
// that dies mid-read, depending on the mode the test sets.
type switchAgent struct {
	mode atomic.Int32
	srv  *httptest.Server
}

func newSwitchAgent(t *testing.T) *switchAgent {
	t.Helper()
	a := &switchAgent{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch agentMode(a.mode.Load()) {
		case modeOversize:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(paddedStatusBody(maxAgentStatusBodyBytes + 1024))
		case modeDrop:
			w.WriteHeader(http.StatusServiceUnavailable)
		case modeReadError:
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write([]byte(`{"agent": {"vers`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(richTelemetry())
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *switchAgent) set(m agentMode) { a.mode.Store(int32(m)) }

// oversizeRouter builds a router with one node per name behind one agent host.
func oversizeRouter(t *testing.T, a *switchAgent, names ...string) *Router {
	t.Helper()
	psSrv := nodePSServer()
	t.Cleanup(psSrv.Close)
	var cfgs []config.NodeConfig
	for _, n := range names {
		cfgs = append(cfgs, config.NodeConfig{Name: n, URL: psSrv.URL})
	}
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, cfgs, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, a.srv.URL), "tok", "http")
	return r
}

func TestOversizeReplyKeepsNodeReachable(t *testing.T) {
	logBuf := captureLog(t)
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	a.set(modeOversize)
	polls := r.healthFailureThreshold + 2
	for i := 0; i < polls; i++ {
		r.pollAgentHosts()
	}
	n.mu.RLock()
	present, failures, stale, unknown := n.AgentPresent, n.AgentFailures, n.AgentStale, n.AgentTelemetryUnknown
	n.mu.RUnlock()
	if !present || failures != 0 || stale || !unknown {
		t.Errorf("present=%v failures=%d stale=%v unknown=%v, want true/0/false/true", present, failures, stale, unknown)
	}
	if got := strings.Count(logBuf.String(), "status response exceeds"); got != 1 {
		t.Errorf("oversize logged %d times across %d polls, want 1\n%s", got, polls, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "stays reachable") {
		t.Errorf("log should say the node stays reachable\n%s", logBuf.String())
	}
	// Telemetry unknown must not mean "do not route".
	r.pollNode(n)
	if picked, _, _ := r.Route("some-model", "", ""); picked == nil || picked.Name != "gpu-0" {
		t.Errorf("Route picked %v, want gpu-0 while its agent report is oversize", picked)
	}
}

// TestReadErrorCounterFactual proves the test above discriminates: the same
// number of polls through the read-error path does mark the agent stale.
func TestReadErrorCounterFactual(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	a.set(modeReadError)
	for i := 0; i < r.healthFailureThreshold+2; i++ {
		r.pollAgentHosts()
	}
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.AgentStale || n.AgentPresent {
		t.Errorf("stale=%v present=%v after read errors, want true/false", n.AgentStale, n.AgentPresent)
	}
}

func TestOversizeAfterGoodPollClearsReadingsKeepsIdentity(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	n.mu.Lock()
	n.VRAMTotalMBConfig = 24000
	n.DeclaredGPUIndices = []int{0}
	n.AgentRuntimeID = "rt-1"
	n.mu.Unlock()
	r.pollAgentHosts()

	n.mu.RLock()
	if n.VRAMSource != "agent" || n.Temperature == nil || n.UptimeSeconds == 0 || n.EngineRunningRequests == nil || len(n.AgentGPUs) == 0 {
		t.Fatalf("good poll did not populate the node (src=%q)", n.VRAMSource)
	}
	detected := n.DetectedParallelismType
	n.mu.RUnlock()

	a.set(modeOversize)
	r.pollAgentHosts()

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource != "declared" || n.VRAMTotalMB != 24000 || n.VRAMUsedMB != 0 {
		t.Errorf("VRAM source=%q total=%d used=%d, want declared/24000/0", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB)
	}
	if n.Temperature != nil || n.PowerDrawW != 0 || n.FanPercent != nil || n.CPUPercent != 0 {
		t.Errorf("live readings not cleared: temp=%v power=%v fan=%v cpu=%v", n.Temperature, n.PowerDrawW, n.FanPercent, n.CPUPercent)
	}
	if n.EngineRunningRequests != nil || n.EngineWaitingRequests != nil || n.EngineKVCacheUsagePercent != nil || n.EnginePrefixCacheHitRatePercent != nil {
		t.Error("engine readings not cleared")
	}
	if n.AgentGPUs != nil || n.RuntimeStatus != "" {
		t.Errorf("per-device readings / runtime status not cleared: gpus=%v status=%q", n.AgentGPUs, n.RuntimeStatus)
	}
	if n.UptimeSeconds != 0 || n.BootTime != 0 {
		t.Errorf("uptime=%d boot=%d, want both cleared", n.UptimeSeconds, n.BootTime)
	}
	if n.AgentVersion != "v0.24.0" || len(n.AgentCapabilities) != 2 || n.AgentRuntimeID != "rt-1" || n.AgentNodeID != "id-1" {
		t.Errorf("identity not preserved: ver=%q caps=%v rt=%q id=%q", n.AgentVersion, n.AgentCapabilities, n.AgentRuntimeID, n.AgentNodeID)
	}
	if n.AgentPlatform != "linux" || n.DriverVersion != "550" || n.Hostname != "gpu-host" || n.AgentRuntime != "vllm" {
		t.Error("static agent metadata not preserved")
	}
	if len(n.DeclaredGPUIndices) != 1 || n.VRAMTotalMBConfig != 24000 {
		t.Error("declared configuration not preserved")
	}
	if n.DetectedParallelismType != detected || detected == "" {
		t.Errorf("detected parallelism %q, want last-known %q kept", n.DetectedParallelismType, detected)
	}
	// Disk/RAM clear as a unit so disk-fit stays "unknown", never "insufficient".
	if n.DiskFreeGB != 0 || n.DiskTotalGB != 0 || n.RAMUsedMB != 0 || n.RAMTotalMB != 0 {
		t.Errorf("disk/ram not cleared together: %v %v %d %d", n.DiskFreeGB, n.DiskTotalGB, n.RAMUsedMB, n.RAMTotalMB)
	}
	if !n.AgentPresent || n.AgentStale {
		t.Error("AgentPresent must stay true and AgentStale false")
	}
}

func TestOversizeWithoutDeclaredVRAMFallsToNone(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	r.pollAgentHosts()
	a.set(modeOversize)
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource != "none" || n.VRAMTotalMB != 0 {
		t.Errorf("VRAM source=%q total=%d, want none/0", n.VRAMSource, n.VRAMTotalMB)
	}
}

func TestOversizeLeavesNonAgentVRAMReadingsAlone(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	temp := 60.0
	n.mu.Lock()
	n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB = "nvidia", 12000, 3000
	n.Temperature, n.PowerDrawW = &temp, 150
	n.mu.Unlock()
	a.set(modeOversize)
	r.pollAgentHosts()
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource != "nvidia" || n.VRAMTotalMB != 12000 || n.VRAMUsedMB != 3000 || n.Temperature == nil || n.PowerDrawW != 150 {
		t.Errorf("local-sourced readings were touched: src=%q total=%d used=%d temp=%v power=%v", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, n.Temperature, n.PowerDrawW)
	}
}

func TestOversizeAfterStaleRecoversAndFiresAgentUpOnce(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p map[string]string
		_ = json.Unmarshal(body, &p)
		mu.Lock()
		received = append(received, p["event"])
		mu.Unlock()
	}))
	defer whSrv.Close()
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	r.SetWebhookConfig(config.WebhookConfig{Enabled: true, URL: whSrv.URL})
	r.pollAgentHosts()
	a.set(modeDrop)
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.pollAgentHosts()
	}
	r.nodes[0].mu.RLock()
	if !r.nodes[0].AgentStale {
		t.Fatal("setup: agent should be stale after repeated failures")
	}
	r.nodes[0].mu.RUnlock()

	a.set(modeOversize)
	r.pollAgentHosts()
	r.pollAgentHosts()

	n := r.nodes[0]
	n.mu.RLock()
	ok := !n.AgentStale && n.AgentPresent && n.AgentFailures == 0
	n.mu.RUnlock()
	if !ok {
		t.Error("oversize reply after stale should clear stale and restore presence")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	up := 0
	for _, e := range received {
		if e == "agent_up" {
			up++
		}
	}
	if up != 1 {
		t.Errorf("agent_up fired %d times, want 1 (events %v)", up, received)
	}
}

func TestOversizeAlternatingWithFailureResetsCounter(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	for i := 0; i < r.healthFailureThreshold+2; i++ {
		a.set(modeDrop)
		r.pollAgentHosts()
		a.set(modeOversize)
		r.pollAgentHosts()
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentStale || n.AgentFailures != 0 || !n.AgentPresent {
		t.Errorf("stale=%v failures=%d present=%v, want false/0/true for a flapping oversize agent", n.AgentStale, n.AgentFailures, n.AgentPresent)
	}
}

func TestOversizeMalformedJSONAndReadErrorStayUnreachable(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	a.set(modeReadError)
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentFailures != 1 || n.AgentTelemetryUnknown {
		t.Errorf("failures=%d unknown=%v, want 1/false for a read error", n.AgentFailures, n.AgentTelemetryUnknown)
	}
}

func TestOversizeSharedHostBothMembersCleared(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	r.pollAgentHosts()
	a.set(modeOversize)
	r.pollAgentHosts()
	for _, n := range r.nodes {
		n.mu.RLock()
		ok := n.AgentPresent && !n.AgentStale && n.AgentTelemetryUnknown && n.AgentFailures == 0 && n.Temperature == nil
		n.mu.RUnlock()
		if !ok {
			t.Errorf("node %s not scoped-cleared and reachable", n.Name)
		}
	}
}

func TestOversizeDropsHostEvidenceButKeepsLogKey(t *testing.T) {
	logBuf := captureLog(t)
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	host := r.nodes[0].Host
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()[host]; !ok {
		t.Fatal("setup: evidence should exist after a good poll")
	}
	a.set(modeOversize)
	for i := 0; i < 4; i++ {
		r.pollAgentHosts()
		if _, ok := r.snapshotHostEvidence()[host]; ok {
			t.Fatalf("evidence still present after oversize poll %d", i+1)
		}
	}
	if got := strings.Count(logBuf.String(), "status response exceeds"); got != 1 {
		t.Errorf("oversize logged %d times, want 1 (log key must survive the evidence drop)", got)
	}
	r.hostEvidenceMu.RLock()
	_, kept := r.hostLogAt["oversize:"+host]
	r.hostEvidenceMu.RUnlock()
	if !kept {
		t.Error("oversize log key was reset by the evidence drop")
	}
}

func TestOversizeAllNodesStillRoutableAndFitUnknown(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	r.pollAgentHosts()
	a.set(modeOversize)
	r.pollAgentHosts()
	for _, n := range r.nodes {
		r.pollNode(n)
	}
	if picked, _, _ := r.Route("some-model", "", ""); picked == nil {
		t.Error("router refused to route when every agent report is oversize")
	}
	if r.ModelFitsAnyHealthyNode("some-model", 0) {
		t.Error("ModelFitsAnyHealthyNode = true with unknown capacity everywhere, want false (not confirmed to fit)")
	}
}

func TestOversizeEvictForHeadroomZeroTotalIsSafe(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	r.pollAgentHosts()
	a.set(modeOversize)
	r.pollAgentHosts()
	if got := r.EvictForHeadroom(context.Background(), "gpu-0", "m", 1<<30); got != 0 {
		t.Errorf("EvictForHeadroom evicted %d with unknown capacity, want 0", got)
	}
}

func TestOversizeThenPollNodeAndConcurrentPollsAreStable(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	a.set(modeOversize)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.pollAgentHosts() }()
		go func() { defer wg.Done(); r.pollNode(r.nodes[0]) }()
	}
	wg.Wait()
	for _, n := range r.nodes {
		n.mu.RLock()
		ok := n.AgentPresent && n.AgentTelemetryUnknown && !n.AgentStale
		n.mu.RUnlock()
		if !ok {
			t.Errorf("node %s unstable after concurrent oversize polls", n.Name)
		}
	}
}

func TestOversizeThenGoodClearsUnknownFlag(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	a.set(modeOversize)
	r.pollAgentHosts()
	a.set(modeGood)
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentTelemetryUnknown || n.VRAMSource != "agent" || n.Temperature == nil {
		t.Errorf("recovery did not restore live telemetry: unknown=%v src=%q temp=%v", n.AgentTelemetryUnknown, n.VRAMSource, n.Temperature)
	}
}
