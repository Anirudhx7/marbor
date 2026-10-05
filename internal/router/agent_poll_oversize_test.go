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
	modeGarbage   // oversize body that is not a JSON object
	modeMalformed // small body that is not valid JSON
)

var (
	oversizeBodyOnce sync.Once
	oversizeBodyData []byte
	garbageBodyOnce  sync.Once
	garbageBodyData  []byte
)

// oversizeBody is the valid-but-oversize status body, built once per test
// binary rather than per request.
func oversizeBody() []byte {
	oversizeBodyOnce.Do(func() { oversizeBodyData = paddedStatusBody(maxAgentStatusBodyBytes + 1024) })
	return oversizeBodyData
}

// garbageBody is an oversize reply that is not a JSON object, built once.
func garbageBody() []byte {
	garbageBodyOnce.Do(func() {
		garbageBodyData = []byte("<html>" + strings.Repeat("x", maxAgentStatusBodyBytes+1024) + "</html>")
	})
	return garbageBodyData
}

// richTelemetry is a status report that populates every reading the oversize
// path is expected to forget and every identity field it must keep.
func richTelemetry() marboragent.Telemetry {
	temp, fan, power, cpu := 71.0, 55.0, 280.0, 33.0
	running, waiting, kv := 2, 1, 40.0
	pq, ph := 100.0, 40.0
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
			Engine: &marboragent.EngineState{RunningRequests: &running, WaitingRequests: &waiting, KVCacheUsagePercent: &kv, PrefixCacheQueries: &pq, PrefixCacheHits: &ph},
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
			_, _ = w.Write(oversizeBody())
		case modeGarbage:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(garbageBody())
		case modeMalformed:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agent": {"version": `))
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
	r, _ := oversizeRouterPS(t, a, names...)
	return r
}

// oversizeRouterPS is oversizeRouter that also returns the nodes' runtime
// server, so a test can take the nodes themselves down.
func oversizeRouterPS(t *testing.T, a *switchAgent, names ...string) (*Router, *httptest.Server) {
	t.Helper()
	psSrv := nodePSServer()
	t.Cleanup(psSrv.Close)
	var cfgs []config.NodeConfig
	for _, n := range names {
		cfgs = append(cfgs, config.NodeConfig{Name: n, URL: psSrv.URL})
	}
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, cfgs, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, a.srv.URL), "tok", "http")
	return r, psSrv
}

func TestLooksLikeJSONObject(t *testing.T) {
	cases := map[string]bool{
		`{"a":1}`:        true,
		" \t\r\n{ }":     true,
		"<html>":         false,
		"[1,2]":          false,
		"":               false,
		"   \n":          false,
		"garbage {":      false,
		"\xef\xbb\xbf{}": false,
	}
	for in, want := range cases {
		if got := looksLikeJSONObject([]byte(in)); got != want {
			t.Errorf("looksLikeJSONObject(%q) = %v, want %v", in, got, want)
		}
	}
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
	if !strings.Contains(logBuf.String(), "telemetry is unknown") {
		t.Errorf("log should say telemetry is unknown\n%s", logBuf.String())
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

// TestOversizeRouteDiscriminatesFromUnreachableNode proves the Route assertion
// can fail: Route returns nil once the node itself is down, so a non-nil pick
// under an oversize agent reply is a real signal. (Agent staleness alone never
// gates Route; node health does.)
func TestOversizeRouteDiscriminatesFromUnreachableNode(t *testing.T) {
	a := newSwitchAgent(t)
	r, psSrv := oversizeRouterPS(t, a, "gpu-0")
	n := r.nodes[0]
	a.set(modeOversize)
	r.pollAgentHosts()
	r.pollNode(n)
	if picked, _, _ := r.Route("some-model", "", ""); picked == nil {
		t.Fatal("Route returned nil for a reachable node with an oversize agent reply")
	}
	psSrv.Close()
	for i := 0; i < r.healthFailureThreshold+2; i++ {
		r.pollNode(n)
	}
	if picked, _, _ := r.Route("some-model", "", ""); picked != nil {
		t.Errorf("Route picked %s for a node whose runtime is down, want nil", picked.Name)
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
	if n.DiskFreeGB == 0 || n.DiskTotalGB == 0 || n.RAMUsedMB == 0 || n.RAMTotalMB == 0 || n.BootTime == 0 ||
		n.FanPercent == nil || n.CPUPercent == 0 || n.PowerDrawW == 0 || n.AgentTelemetryUnknown ||
		n.prevEnginePrefixCacheQueries == nil || n.prevEnginePrefixCacheHits == nil {
		t.Fatalf("good poll left a precondition reading unset: disk=%v/%v ram=%d/%d boot=%d fan=%v cpu=%v power=%v unknown=%v",
			n.DiskFreeGB, n.DiskTotalGB, n.RAMUsedMB, n.RAMTotalMB, n.BootTime, n.FanPercent, n.CPUPercent, n.PowerDrawW, n.AgentTelemetryUnknown)
	}
	detected := n.DetectedParallelismType
	gpuCount := n.AgentGPUCount
	n.mu.RUnlock()

	a.set(modeOversize)
	r.pollAgentHosts()

	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.AgentTelemetryUnknown {
		t.Error("AgentTelemetryUnknown = false after an oversize reply, want true")
	}
	if n.prevEnginePrefixCacheQueries != nil || n.prevEnginePrefixCacheHits != nil {
		t.Error("previous prefix-cache counters not cleared")
	}
	if n.AgentGPUCount != gpuCount || gpuCount == 0 {
		t.Errorf("AgentGPUCount = %d, want static identity %d kept", n.AgentGPUCount, gpuCount)
	}
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
	// Disk/RAM clear as a unit. DiskTotalGB == 0 is what internal/admin's
	// classifyDiskFit/diskTelemetryUnknown read as "unknown", so the disk-fit
	// check never reports a fabricated "insufficient".
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

// TestOversizeNonAgentVRAMKeepsVRAMButClearsAgentTempAndPower covers a node
// whose VRAM is not agent-sourced (declared, API-derived, none): the agent
// was still the only source of its temperature and power, so those clear,
// while the VRAM figure that is not the agent's stays.
func TestOversizeNonAgentVRAMKeepsVRAMButClearsAgentTempAndPower(t *testing.T) {
	for _, src := range []string{"declared", "api", "none"} {
		t.Run(src, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := oversizeRouter(t, a, "gpu-0")
			n := r.nodes[0]
			temp := 66.0
			n.mu.Lock()
			n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB = src, 20000, 5000
			n.Temperature, n.PowerDrawW = &temp, 210
			n.mu.Unlock()
			a.set(modeOversize)
			r.pollAgentHosts()
			n.mu.RLock()
			defer n.mu.RUnlock()
			if n.Temperature != nil || n.PowerDrawW != 0 {
				t.Errorf("temp=%v power=%v, want cleared for %s-sourced node", n.Temperature, n.PowerDrawW, src)
			}
			if n.VRAMSource != src || n.VRAMTotalMB != 20000 || n.VRAMUsedMB != 5000 {
				t.Errorf("VRAM src=%q total=%d used=%d, want %s/20000/5000 untouched", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, src)
			}
		})
	}
}

func TestOversizeLeavesLocalNvidiaReadingsAlone(t *testing.T) {
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
	events := make(chan string, 16)
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p map[string]string
		_ = json.Unmarshal(body, &p)
		events <- p["event"]
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
	n := r.nodes[0]
	n.mu.RLock()
	stale := n.AgentStale
	n.mu.RUnlock()
	if !stale {
		t.Fatal("setup: agent should be stale after repeated failures")
	}

	a.set(modeOversize)
	r.pollAgentHosts()
	r.pollAgentHosts()

	n.mu.RLock()
	ok := !n.AgentStale && n.AgentPresent && n.AgentFailures == 0
	n.mu.RUnlock()
	if !ok {
		t.Error("oversize reply after stale should clear stale and restore presence")
	}

	// Wait (bounded) for both transitions, then briefly for any extra event.
	got := map[string]int{}
	timeout := time.After(3 * time.Second)
	for got["agent_down"] < 1 || got["agent_up"] < 1 {
		select {
		case e := <-events:
			got[e]++
		case <-timeout:
			t.Fatalf("webhook events after 3s: %v, want one agent_down and one agent_up", got)
		}
	}
	settle := time.After(200 * time.Millisecond)
	for done := false; !done; {
		select {
		case e := <-events:
			got[e]++
		case <-settle:
			done = true
		}
	}
	if got["agent_down"] != 1 || got["agent_up"] != 1 {
		t.Errorf("events %v, want exactly one agent_down and one agent_up", got)
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

// TestOversizeBadReplyShapesStayUnreachable covers the replies that must NOT
// count as "agent reachable, telemetry unknown": an unreadable body, a small
// malformed body, and an oversize body that is not a JSON object.
func TestOversizeBadReplyShapesStayUnreachable(t *testing.T) {
	cases := []struct {
		name string
		mode agentMode
	}{
		{"read error", modeReadError},
		{"malformed json", modeMalformed},
		{"oversize not a json object", modeGarbage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := oversizeRouter(t, a, "gpu-0")
			r.pollAgentHosts() // good poll: AgentPresent is true going in
			a.set(tc.mode)
			r.pollAgentHosts()
			n := r.nodes[0]
			n.mu.RLock()
			defer n.mu.RUnlock()
			if n.AgentFailures != 1 || n.AgentTelemetryUnknown || !n.AgentPresent {
				t.Errorf("failures=%d unknown=%v present=%v, want 1/false/true (one failed poll, below the stale threshold)", n.AgentFailures, n.AgentTelemetryUnknown, n.AgentPresent)
			}
		})
	}
}

func TestOversizeNotJSONObjectLogsOnceAndNeverFlagsUnknown(t *testing.T) {
	logBuf := captureLog(t)
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	a.set(modeGarbage)
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.pollAgentHosts()
	}
	n := r.nodes[0]
	n.mu.RLock()
	stale, unknown := n.AgentStale, n.AgentTelemetryUnknown
	n.mu.RUnlock()
	if !stale || unknown {
		t.Errorf("stale=%v unknown=%v, want true/false once a non-object oversize reply repeats", stale, unknown)
	}
	if got := strings.Count(logBuf.String(), "is not a JSON object"); got != 1 {
		t.Errorf("not-an-object logged %d times, want 1 (rate-limited)\n%s", got, logBuf.String())
	}
}

func TestOversizeSharedHostBothMembersCleared(t *testing.T) {
	logBuf := captureLog(t)
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	host := r.nodes[0].Host
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()[host]; !ok {
		t.Fatal("setup: evidence should exist after a good poll")
	}
	a.set(modeOversize)
	r.pollAgentHosts()
	// The host's evidence is dropped once for the host, not once per member,
	// and the oversize warning is one line for the host.
	if _, ok := r.snapshotHostEvidence()[host]; ok {
		t.Error("host evidence still present after an oversize poll")
	}
	if got := strings.Count(logBuf.String(), "status response exceeds"); got != 1 {
		t.Errorf("oversize logged %d times for a 2-member host, want 1", got)
	}
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
	n := r.nodes[0]
	n.mu.Lock()
	n.LoadedModels = []ModelInfo{{Name: "resident", SizeVRAM: 4 << 30}}
	total := n.VRAMTotalMB
	n.mu.Unlock()
	if total != 0 {
		t.Fatalf("setup: VRAMTotalMB = %d, want 0 after an oversize reply", total)
	}
	if got := r.EvictForHeadroom(context.Background(), "gpu-0", "m", 1<<30); got != 0 {
		t.Errorf("EvictForHeadroom evicted %d with unknown capacity, want 0", got)
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.LoadedModels) != 1 {
		t.Errorf("loaded models = %d, want the resident model untouched", len(n.LoadedModels))
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

// TestClearAgentTelemetryResetsUnknownFlag: an agent that goes dark after an
// oversize reply is plain unreachable, not "telemetry unknown".
func TestClearAgentTelemetryResetsUnknownFlag(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	a.set(modeOversize)
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	setUp := n.AgentTelemetryUnknown
	n.mu.RUnlock()
	if !setUp {
		t.Fatal("setup: AgentTelemetryUnknown should be set")
	}
	a.set(modeDrop)
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.pollAgentHosts()
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentTelemetryUnknown || n.AgentPresent || !n.AgentStale {
		t.Errorf("unknown=%v present=%v stale=%v, want false/false/true once the agent goes dark", n.AgentTelemetryUnknown, n.AgentPresent, n.AgentStale)
	}
}

// TestOversizeClearsTLSMismatch: a reply that arrives means the TLS handshake
// succeeded, so a previously set mismatch flag must not linger.
func TestOversizeClearsTLSMismatch(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	r.setAgentTLSMismatch(n, true)
	a.set(modeOversize)
	r.pollAgentHosts()
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentTLSMismatch {
		t.Error("AgentTLSMismatch still set after an oversize reply")
	}
}

// TestApplyAgentTelemetryGPULessBranchFallsBackAgentVRAM drives the branch of
// applyAgentTelemetry where the agent knows the GPU vendor but reports no
// devices: agent-sourced VRAM must fall back to the declared figure.
func TestApplyAgentTelemetryGPULessBranchFallsBackAgentVRAM(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	n.mu.Lock()
	n.VRAMTotalMBConfig = 24000
	n.mu.Unlock()
	r.pollAgentHosts()
	tel := richTelemetry()
	tel.GPU.Devices = nil
	r.applyAgentTelemetry(n, tel)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource != "declared" || n.VRAMTotalMB != 24000 || n.VRAMUsedMB != 0 || n.Temperature != nil || n.PowerDrawW != 0 {
		t.Errorf("src=%q total=%d used=%d temp=%v power=%v, want declared/24000/0/nil/0", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, n.Temperature, n.PowerDrawW)
	}
}

// mixedFleet builds an agent-sourced node ("gpu-0", on the agent host) and a
// healthy peer on a different host with the same declared VRAM. The agent
// has already reported once, so gpu-0's VRAM was agent-sourced before its
// reply went oversize.
func mixedFleet(t *testing.T) (*Router, *NodeState, *NodeState) {
	t.Helper()
	a := newSwitchAgent(t)
	psA, psB := nodePSServer(), nodePSServer()
	t.Cleanup(psA.Close)
	t.Cleanup(psB.Close)
	peerURL := strings.Replace(psB.URL, "127.0.0.1", "localhost", 1)
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psA.URL},
		{Name: "peer", URL: peerURL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, a.srv.URL), "tok", "http")
	r.pollAgentHosts()
	peer := r.nodes[1]
	peer.mu.Lock()
	peer.VRAMSource, peer.VRAMTotalMB, peer.VRAMUsedMB = "declared", 16000, 8000
	peer.mu.Unlock()
	a.set(modeOversize)
	r.pollAgentHosts()
	return r, r.nodes[0], peer
}

// TestOversizeMixedFleetPlacement pins what placement really does with an
// oversize agent-sourced node next to a healthy peer: its VRAM reads as zero,
// so the peer wins on score, but the node stays selectable whenever the peer
// is unavailable or busy.
func TestOversizeMixedFleetPlacement(t *testing.T) {
	t.Run("peer wins on score", func(t *testing.T) {
		r, n, _ := mixedFleet(t)
		n.mu.RLock()
		src, total := n.VRAMSource, n.VRAMTotalMB
		n.mu.RUnlock()
		if src != "none" || total != 0 {
			t.Fatalf("setup: oversize node VRAM src=%q total=%d, want none/0", src, total)
		}
		if picked, _, _ := r.Route("some-model", "", ""); picked == nil || picked.Name != "peer" {
			t.Errorf("Route picked %v, want the peer with known VRAM", picked)
		}
	})
	t.Run("selectable when the peer is unhealthy", func(t *testing.T) {
		r, _, peer := mixedFleet(t)
		peer.mu.Lock()
		peer.Healthy = false
		peer.mu.Unlock()
		if picked, _, _ := r.Route("some-model", "", ""); picked == nil || picked.Name != "gpu-0" {
			t.Errorf("Route picked %v, want gpu-0 when the peer is unhealthy", picked)
		}
	})
	t.Run("selectable when the peer is at capacity", func(t *testing.T) {
		r, _, peer := mixedFleet(t)
		peer.mu.Lock()
		peer.MaxInFlight = 1
		peer.mu.Unlock()
		atomic.StoreInt32(&peer.ActiveConns, 1)
		if picked, _, _ := r.Route("some-model", "", ""); picked == nil || picked.Name != "gpu-0" {
			t.Errorf("Route picked %v, want gpu-0 when the peer is full", picked)
		}
	})
}

// TestOversizeConfirmedReplicaGroupStaysResolved: a declared, mutually
// confirmed replica group keeps its head/worker roles when the host evidence
// is dropped on an oversize reply; roles come from the declarations, not from
// the dropped evidence.
func TestOversizeConfirmedReplicaGroupStaysResolved(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	for _, n := range r.nodes {
		n.mu.Lock()
		n.ReplicaPeers = peers("gpu-0", "gpu-0", "gpu-1")
		n.mu.Unlock()
	}
	r.pollAgentHosts()
	before := r.resolveSchedulingRoles(r.Nodes())
	if before["gpu-0"] != RoleHead || before["gpu-1"] != RoleWorker {
		t.Fatalf("setup: roles %v, want gpu-0 head and gpu-1 worker", before)
	}
	a.set(modeOversize)
	r.pollAgentHosts()
	after := r.resolveSchedulingRoles(r.Nodes())
	if after["gpu-0"] != RoleHead || after["gpu-1"] != RoleWorker {
		t.Errorf("roles after oversize %v, want unchanged head/worker", after)
	}
	for _, n := range r.nodes {
		r.pollNode(n)
	}
	if picked, _, _ := r.Route("some-model", "", ""); picked == nil || picked.Name != "gpu-0" {
		t.Errorf("Route picked %v, want the group head gpu-0", picked)
	}
}
