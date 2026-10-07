package router

import (
	"context"
	"encoding/json"
	"fmt"
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
	// modeOversize500 answers 500 with an oversize JSON-object body: a failed
	// status wins over the body shape.
	modeOversize500
	// modeEndless streams an endless JSON-object-shaped body until the client
	// goes away.
	modeEndless
	modeOversizeArray     // oversize body that starts like a JSON array
	modeOversizeBraceJunk // oversize body that starts with '{' but is not JSON
	modeOversizeLeadingWS // oversize JSON object preceded by leading whitespace
)

var (
	oversizeBodyOnce sync.Once
	oversizeBodyData []byte
	garbageBodyOnce  sync.Once
	garbageBodyData  []byte
	shapeBodyOnce    sync.Once
	arrayBodyData    []byte
	braceJunkData    []byte
	leadingWSData    []byte
)

// shapeBodies builds the oversize bodies used by the shape tests, once.
func shapeBodies() (array, braceJunk, leadingWS []byte) {
	shapeBodyOnce.Do(func() {
		pad := maxAgentStatusBodyBytes + 1024
		arrayBodyData = []byte("[1,2," + strings.Repeat("3,", pad/2) + "4]")
		braceJunkData = []byte("{garbage" + strings.Repeat("x", pad))
		leadingWSData = append([]byte(" \t\r\n"), paddedStatusBody(pad)...)
	})
	return arrayBodyData, braceJunkData, leadingWSData
}

// countHostLogLines counts captured log lines that mention the quoted host and
// carry token. Counting by host plus a short token keeps tests off the exact
// wording of a message.
func countHostLogLines(out, host, token string) int {
	quoted := fmt.Sprintf("%q", host)
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, quoted) && strings.Contains(line, token) {
			n++
		}
	}
	return n
}

// webhookRecorder collects webhook events under a mutex and wakes waiters per
// event, so a test can wait for one event with its own deadline and then check
// a sentinel barrier plus a short settle for any late duplicate.
type webhookRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	counts map[string]int
	wake   chan struct{}
}

func newWebhookRecorder(t *testing.T) *webhookRecorder {
	t.Helper()
	w := &webhookRecorder{counts: map[string]int{}, wake: make(chan struct{}, 64)}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p map[string]string
		_ = json.Unmarshal(body, &p)
		w.mu.Lock()
		w.counts[p["event"]]++
		w.mu.Unlock()
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *webhookRecorder) count(event string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.counts[event]
}

func (w *webhookRecorder) snapshot() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.counts))
	for k, v := range w.counts {
		out[k] = v
	}
	return out
}

// waitFor blocks until event has been seen at least want times or d elapses.
func (w *webhookRecorder) waitFor(event string, want int, d time.Duration) bool {
	deadline := time.After(d)
	for w.count(event) < want {
		select {
		case <-w.wake:
		case <-deadline:
			return w.count(event) >= want
		}
	}
	return true
}

// barrier fires a sentinel webhook through r and waits until the recorder has
// seen it, then lets a short settle pass before returning every event seen. All
// events the polls queued were started before the sentinel, so their deliveries
// have normally landed by the time it does. Delivery is unordered (each webhook
// is its own goroutine and the router has no synchronous seam), so the sentinel
// narrows the window for a late duplicate rather than eliminating it; the settle
// covers the remainder. The sentinel wait is what bounds the check: if it never
// arrives the test fails instead of passing on slow delivery.
func (w *webhookRecorder) barrier(t *testing.T, r *Router) map[string]int {
	t.Helper()
	r.fireWebhook("barrier", "gpu-0", "")
	if !w.waitFor("barrier", 1, 3*time.Second) {
		t.Fatalf("sentinel webhook not delivered within 3s; events so far %v", w.snapshot())
	}
	time.Sleep(webhookSettle)
	return w.snapshot()
}

const webhookSettle = 150 * time.Millisecond

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
			Caps:        &marboragent.RuntimeCaps{TP: true, MaxTPWidth: 8},
			Source:      "ps",
		}},
	}
}

// switchAgent serves its telemetry, an oversize padded body, a 503, or a body
// that dies mid-read, depending on the mode the test sets.
type switchAgent struct {
	mode atomic.Int32
	srv  *httptest.Server
	tel  marboragent.Telemetry
	// endlessWritten counts the bytes modeEndless has streamed; endlessDone is
	// closed when that handler returns.
	endlessWritten atomic.Int64
	endlessDone    chan struct{}
}

func newSwitchAgent(t *testing.T) *switchAgent {
	t.Helper()
	return newSwitchAgentWith(t, richTelemetry())
}

// newSwitchAgentWith serves tel in good mode. tel is set before the server
// starts and never changed afterwards.
func newSwitchAgentWith(t *testing.T, tel marboragent.Telemetry) *switchAgent {
	t.Helper()
	a := &switchAgent{tel: tel, endlessDone: make(chan struct{})}
	var endlessOnce sync.Once
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch agentMode(a.mode.Load()) {
		case modeOversize:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(oversizeBody())
		case modeOversize500:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(oversizeBody())
		case modeOversizeArray:
			arr, _, _ := shapeBodies()
			_, _ = w.Write(arr)
		case modeOversizeBraceJunk:
			_, junk, _ := shapeBodies()
			_, _ = w.Write(junk)
		case modeOversizeLeadingWS:
			_, _, ws := shapeBodies()
			_, _ = w.Write(ws)
		case modeEndless:
			defer endlessOnce.Do(func() { close(a.endlessDone) })
			pad := []byte(strings.Repeat(" ", 64<<10))
			chunk := append([]byte(`{"agent":`), pad...)
			for req.Context().Err() == nil {
				n, err := w.Write(chunk)
				a.endlessWritten.Add(int64(n))
				if err != nil {
					return
				}
				chunk = pad
			}
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
			_ = json.NewEncoder(w).Encode(a.tel)
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
		// Only space, tab, CR and LF are JSON whitespace; anything else before
		// the brace is not a JSON object.
		"\x00{}": false,
		"\f{}":   false,
		"\v{}":   false,
		// A lone brace has the object shape; the check is deliberately cheap.
		"{": true,
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
	// One warning for the host across every poll, counted by host plus a
	// stable token rather than the message wording.
	host := n.Host
	out := logBuf.String()
	if got := countHostLogLines(out, host, "exceeds"); got != 1 {
		t.Errorf("oversize logged %d lines for host %q across %d polls, want 1\n%s", got, host, polls, out)
	}
	if got := countHostLogLines(out, host, "telemetry is unknown"); got < 1 {
		t.Errorf("no log line for host %q says telemetry is unknown\n%s", host, out)
	}
	// Smoke check, not a revert detector: Route never reads agent state, so
	// this passes with or without the oversize handling. What it guards is
	// that nothing in the oversize path makes a reachable node unroutable; the
	// state asserts below are the ones that discriminate.
	r.pollNode(n)
	n.mu.RLock()
	present, unknown, src := n.AgentPresent, n.AgentTelemetryUnknown, n.VRAMSource
	n.mu.RUnlock()
	if !present || !unknown || src == "agent" {
		t.Errorf("after pollNode present=%v unknown=%v VRAMSource=%q, want true/true/not agent", present, unknown, src)
	}
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
	src, popOK := n.VRAMSource, n.Temperature != nil && n.UptimeSeconds != 0 && n.EngineRunningRequests != nil && len(n.AgentGPUs) != 0
	diskFree, diskTotal, ramUsed, ramTotal, boot := n.DiskFreeGB, n.DiskTotalGB, n.RAMUsedMB, n.RAMTotalMB, n.BootTime
	fan, cpu, power, unknown := n.FanPercent, n.CPUPercent, n.PowerDrawW, n.AgentTelemetryUnknown
	prevSet := n.prevEnginePrefixCacheQueries != nil && n.prevEnginePrefixCacheHits != nil
	detected := n.DetectedParallelismType
	gpuCount := n.AgentGPUCount
	rtVersion, cudaVersion, gpuVendor, arch := n.RuntimeVersion, n.CUDAVersion, n.AgentGPUVendor, n.AgentArchitecture
	detRuntime, detWidth, detSource := n.DetectedRuntime, n.DetectedParallelismWidth, n.DetectedSource
	var detCaps marboragent.RuntimeCaps
	detCapsSet := n.DetectedCaps != nil
	if detCapsSet {
		detCaps = *n.DetectedCaps
	}
	n.mu.RUnlock()
	if src != "agent" || !popOK {
		t.Fatalf("good poll did not populate the node (src=%q)", src)
	}
	// Every static field compared after the oversize poll must be populated
	// here, or the comparison proves nothing.
	if rtVersion == "" || cudaVersion == "" || gpuVendor == "" || arch == "" ||
		detRuntime == "" || detWidth == 0 || detSource == "" || !detCapsSet || detCaps.MaxTPWidth == 0 {
		t.Fatalf("good poll left a static field unset: runtimeVersion=%q cuda=%q vendor=%q arch=%q detRuntime=%q detWidth=%d detSource=%q detCaps=%v",
			rtVersion, cudaVersion, gpuVendor, arch, detRuntime, detWidth, detSource, detCapsSet)
	}
	if diskFree == 0 || diskTotal == 0 || ramUsed == 0 || ramTotal == 0 || boot == 0 ||
		fan == nil || cpu == 0 || power == 0 || unknown || !prevSet {
		t.Fatalf("good poll left a precondition reading unset: disk=%v/%v ram=%d/%d boot=%d fan=%v cpu=%v power=%v unknown=%v prev=%v",
			diskFree, diskTotal, ramUsed, ramTotal, boot, fan, cpu, power, unknown, prevSet)
	}

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
	if n.RuntimeVersion != rtVersion {
		t.Errorf("RuntimeVersion = %q, want %q kept", n.RuntimeVersion, rtVersion)
	}
	if n.CUDAVersion != cudaVersion {
		t.Errorf("CUDAVersion = %q, want %q kept", n.CUDAVersion, cudaVersion)
	}
	if n.AgentGPUVendor != gpuVendor {
		t.Errorf("AgentGPUVendor = %q, want %q kept", n.AgentGPUVendor, gpuVendor)
	}
	if n.AgentArchitecture != arch {
		t.Errorf("AgentArchitecture = %q, want %q kept", n.AgentArchitecture, arch)
	}
	if n.DetectedRuntime != detRuntime {
		t.Errorf("DetectedRuntime = %q, want %q kept", n.DetectedRuntime, detRuntime)
	}
	if n.DetectedParallelismWidth != detWidth {
		t.Errorf("DetectedParallelismWidth = %d, want %d kept", n.DetectedParallelismWidth, detWidth)
	}
	if n.DetectedSource != detSource {
		t.Errorf("DetectedSource = %q, want %q kept", n.DetectedSource, detSource)
	}
	if n.DetectedCaps == nil || *n.DetectedCaps != detCaps {
		t.Errorf("DetectedCaps = %v, want %+v kept", n.DetectedCaps, detCaps)
	}
	if len(n.DeclaredGPUIndices) != 1 || n.VRAMTotalMBConfig != 24000 {
		t.Error("declared configuration not preserved")
	}
	if n.DetectedParallelismType != detected || detected == "" {
		t.Errorf("detected parallelism %q, want last-known %q kept", n.DetectedParallelismType, detected)
	}
	// Disk/RAM clear as a unit. DiskTotalGB == 0 is what internal/admin's
	// classifyDiskFit/diskTelemetryUnknown read as "unknown", so the disk-fit
	// check never reports a fabricated "insufficient"; internal/admin's
	// TestClassifyDiskFitOversizeClearedShapeIsUnknown pins that reading.
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
	// A "none" source means no VRAM figure at all, so its total/used are zero;
	// the other sources carry a real figure that must survive untouched.
	for _, tc := range []struct {
		src         string
		total, used int64
	}{{"declared", 20000, 5000}, {"api", 20000, 5000}, {"none", 0, 0}} {
		src := tc.src
		t.Run(src, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := oversizeRouter(t, a, "gpu-0")
			n := r.nodes[0]
			temp := 66.0
			n.mu.Lock()
			n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB = src, tc.total, tc.used
			n.Temperature, n.PowerDrawW = &temp, 210
			n.mu.Unlock()
			a.set(modeOversize)
			r.pollAgentHosts()
			n.mu.RLock()
			defer n.mu.RUnlock()
			if !n.AgentPresent || !n.AgentTelemetryUnknown {
				t.Errorf("present=%v unknown=%v, want true/true for %s-sourced node with an oversize reply", n.AgentPresent, n.AgentTelemetryUnknown, src)
			}
			if n.Temperature != nil || n.PowerDrawW != 0 {
				t.Errorf("temp=%v power=%v, want cleared for %s-sourced node", n.Temperature, n.PowerDrawW, src)
			}
			if n.VRAMSource != src || n.VRAMTotalMB != tc.total || n.VRAMUsedMB != tc.used {
				t.Errorf("VRAM src=%q total=%d used=%d, want %s/%d/%d untouched", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, src, tc.total, tc.used)
			}
		})
	}
}

func TestOversizeLeavesLocalNvidiaReadingsAlone(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	temp, fan, running, waiting, kv := 60.0, 40.0, 3, 2, 55.0
	n.mu.Lock()
	n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB = "nvidia", 12000, 3000
	n.Temperature, n.PowerDrawW = &temp, 150
	n.FanPercent, n.CPUPercent = &fan, 25
	n.EngineRunningRequests, n.EngineWaitingRequests, n.EngineKVCacheUsagePercent = &running, &waiting, &kv
	n.RAMUsedMB, n.RAMTotalMB, n.DiskFreeGB, n.DiskTotalGB = 1000, 8000, 50, 200
	n.mu.Unlock()
	a.set(modeOversize)
	r.pollAgentHosts()
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.AgentTelemetryUnknown || !n.AgentPresent {
		t.Errorf("unknown=%v present=%v, want true/true for a local-nvidia node with an oversize agent reply", n.AgentTelemetryUnknown, n.AgentPresent)
	}
	// Only the agent-derived readings clear; the local nvidia-smi ones stay.
	if n.FanPercent != nil || n.CPUPercent != 0 ||
		n.EngineRunningRequests != nil || n.EngineWaitingRequests != nil || n.EngineKVCacheUsagePercent != nil ||
		n.RAMUsedMB != 0 || n.RAMTotalMB != 0 || n.DiskFreeGB != 0 || n.DiskTotalGB != 0 {
		t.Errorf("agent readings not cleared on a local-nvidia node: fan=%v cpu=%v engine=%v/%v/%v ram=%d/%d disk=%v/%v",
			n.FanPercent, n.CPUPercent, n.EngineRunningRequests, n.EngineWaitingRequests, n.EngineKVCacheUsagePercent,
			n.RAMUsedMB, n.RAMTotalMB, n.DiskFreeGB, n.DiskTotalGB)
	}
	if n.VRAMSource != "nvidia" || n.VRAMTotalMB != 12000 || n.VRAMUsedMB != 3000 || n.Temperature == nil || n.PowerDrawW != 150 {
		t.Errorf("local-sourced readings were touched: src=%q total=%d used=%d temp=%v power=%v", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, n.Temperature, n.PowerDrawW)
	}
}

func TestOversizeAfterStaleRecoversAndFiresAgentUpOnce(t *testing.T) {
	rec := newWebhookRecorder(t)
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	r.SetWebhookConfig(config.WebhookConfig{Enabled: true, URL: rec.srv.URL})
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

	// Each transition gets its own bounded wait.
	for _, ev := range []string{"agent_down", "agent_up"} {
		if !rec.waitFor(ev, 1, 3*time.Second) {
			t.Fatalf("no %s webhook within 3s; events so far %v", ev, rec.snapshot())
		}
	}
	// Then the sentinel barrier bounds the check for a late duplicate.
	if got := rec.barrier(t, r); got["agent_down"] != 1 || got["agent_up"] != 1 {
		t.Errorf("events %v, want exactly one agent_down and one agent_up", got)
	}
}

// TestOversizeFlapFiresNoAgentWebhooks: a reachable agent whose reply is
// oversize is not down, so a good poll followed by repeated oversize polls
// (past the failure threshold) fires neither agent_down nor agent_up.
func TestOversizeFlapFiresNoAgentWebhooks(t *testing.T) {
	rec := newWebhookRecorder(t)
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	r.SetWebhookConfig(config.WebhookConfig{Enabled: true, URL: rec.srv.URL})
	r.pollAgentHosts()
	a.set(modeOversize)
	for i := 0; i < r.healthFailureThreshold+1; i++ {
		r.pollAgentHosts()
	}
	if got := rec.barrier(t, r); got["agent_down"] != 0 || got["agent_up"] != 0 {
		t.Errorf("events %v, want no agent_down and no agent_up for an oversize-but-reachable agent", got)
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
// malformed body, and an oversize body that is not a JSON object. The oversize
// body that starts with '{' but is not JSON is reachable by design and lives in
// TestOversizeObjectShapeRule; non-200 oversize bodies are the 500 case below.
func TestOversizeBadReplyShapesStayUnreachable(t *testing.T) {
	cases := []struct {
		name string
		mode agentMode
	}{
		{"read error", modeReadError},
		{"malformed json", modeMalformed},
		{"oversize not a json object", modeGarbage},
		{"oversize json array", modeOversizeArray},
		{"non-200 with an oversize json object body", modeOversize500},
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
			failures, unknown, present := n.AgentFailures, n.AgentTelemetryUnknown, n.AgentPresent
			src, temp, version, uptime := n.VRAMSource, n.Temperature, n.AgentVersion, n.UptimeSeconds
			n.mu.RUnlock()
			if failures != 1 || unknown || !present {
				t.Errorf("failures=%d unknown=%v present=%v, want 1/false/true (one failed poll, below the stale threshold)", failures, unknown, present)
			}
			// One failed poll keeps the last good readings.
			if src != "agent" || temp == nil || version != "v0.24.0" || uptime == 0 {
				t.Errorf("prior readings lost after one failed poll: src=%q temp=%v version=%q uptime=%d", src, temp, version, uptime)
			}
			// Polling on to the threshold marks the agent stale and drops them.
			for i := 1; i < r.healthFailureThreshold; i++ {
				r.pollAgentHosts()
			}
			n.mu.RLock()
			defer n.mu.RUnlock()
			if !n.AgentStale || n.AgentPresent || n.AgentTelemetryUnknown {
				t.Errorf("stale=%v present=%v unknown=%v at the threshold, want true/false/false", n.AgentStale, n.AgentPresent, n.AgentTelemetryUnknown)
			}
			if n.VRAMSource == "agent" || n.Temperature != nil || n.AgentVersion != "" || n.UptimeSeconds != 0 {
				t.Errorf("readings kept at the threshold: src=%q temp=%v version=%q uptime=%d", n.VRAMSource, n.Temperature, n.AgentVersion, n.UptimeSeconds)
			}
		})
	}
}

// TestOversizeObjectShapeRule pins the cheap shape rule end to end: only the
// first non-whitespace byte decides. An oversize body starting with '[' is
// unreachable; one starting with '{' is reachable with unknown telemetry even
// if the rest is not valid JSON (intentional: the body is too large to decode,
// and an object-like start is the only evidence of a live agent available
// without reading it); leading JSON whitespace before '{' still counts. The
// brace-then-garbage case is therefore reachable on purpose, not a gap; oversize
// bodies behind a non-200 status are covered by the 500 case in
// TestOversizeBadReplyShapesStayUnreachable.
func TestOversizeObjectShapeRule(t *testing.T) {
	cases := []struct {
		name          string
		mode          agentMode
		wantReachable bool
	}{
		{"array", modeOversizeArray, false},
		{"brace then garbage", modeOversizeBraceJunk, true},
		{"leading whitespace then object", modeOversizeLeadingWS, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := oversizeRouter(t, a, "gpu-0")
			r.pollAgentHosts()
			a.set(tc.mode)
			r.pollAgentHosts()
			n := r.nodes[0]
			n.mu.RLock()
			defer n.mu.RUnlock()
			if tc.wantReachable {
				if n.AgentFailures != 0 || !n.AgentPresent || !n.AgentTelemetryUnknown {
					t.Errorf("failures=%d present=%v unknown=%v, want 0/true/true", n.AgentFailures, n.AgentPresent, n.AgentTelemetryUnknown)
				}
				return
			}
			if n.AgentFailures != 1 || n.AgentTelemetryUnknown {
				t.Errorf("failures=%d unknown=%v, want 1/false", n.AgentFailures, n.AgentTelemetryUnknown)
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
	// The host's evidence is gone, and the oversize warning is one line for the
	// host rather than one per member.
	if _, ok := r.snapshotHostEvidence()[host]; ok {
		t.Error("host evidence still present after an oversize poll")
	}
	if got := countHostLogLines(logBuf.String(), host, "exceeds"); got != 1 {
		t.Errorf("oversize logged %d lines for a 2-member host, want 1\n%s", got, logBuf.String())
	}
	for _, n := range r.nodes {
		n.mu.RLock()
		present, stale, unknown, failures, temp := n.AgentPresent, n.AgentStale, n.AgentTelemetryUnknown, n.AgentFailures, n.Temperature
		n.mu.RUnlock()
		if !present {
			t.Errorf("node %s: AgentPresent = false, want true", n.Name)
		}
		if stale {
			t.Errorf("node %s: AgentStale = true, want false", n.Name)
		}
		if !unknown {
			t.Errorf("node %s: AgentTelemetryUnknown = false, want true", n.Name)
		}
		if failures != 0 {
			t.Errorf("node %s: AgentFailures = %d, want 0", n.Name, failures)
		}
		if temp != nil {
			t.Errorf("node %s: Temperature = %v, want nil (cleared)", n.Name, *temp)
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
	// Flap: the agent recovers (evidence is back) and goes oversize again. The
	// warning is still one line in total, observed through the log alone.
	a.set(modeGood)
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()[host]; !ok {
		t.Fatal("evidence should return after the agent recovers")
	}
	a.set(modeOversize)
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()[host]; ok {
		t.Fatal("evidence still present after the second oversize episode")
	}
	if got := countHostLogLines(logBuf.String(), host, "exceeds"); got != 1 {
		t.Errorf("oversize logged %d lines across an oversize/good/oversize flap, want 1 (log limit must survive the evidence drop)\n%s", got, logBuf.String())
	}
}

// TestOversizeAllNodesStillRoutableAndFitUnknown runs the same fleet twice,
// both times through a good poll first: with the agent's capacity known the
// fit check compares real sizes (a model that fits says yes, one that does not
// says no, so the answer is not the unknown-size fallback), with the report
// then oversize (capacity unknown everywhere) it says no to both, though
// routing still works.
func TestOversizeAllNodesStillRoutableAndFitUnknown(t *testing.T) {
	const small, huge = "small-model", "huge-model"
	for _, oversize := range []bool{false, true} {
		name := "capacity known"
		if oversize {
			name = "oversize"
		}
		t.Run(name, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := oversizeRouter(t, a, "gpu-0", "gpu-1")
			r.pollAgentHosts()
			if oversize {
				a.set(modeOversize)
				r.pollAgentHosts()
			}
			for _, n := range r.nodes {
				r.pollNode(n)
			}
			n := r.nodes[0]
			n.mu.RLock()
			src, total := n.VRAMSource, n.VRAMTotalMB
			present, unknown := n.AgentPresent, n.AgentTelemetryUnknown
			n.mu.RUnlock()
			if wantKnown := !oversize; (total > 0) != wantKnown || (wantKnown && src != "agent") {
				t.Fatalf("setup: VRAM src=%q total=%d, oversize=%v", src, total, oversize)
			}
			if !present || unknown != oversize {
				t.Errorf("AgentPresent=%v AgentTelemetryUnknown=%v, want true/%v", present, unknown, oversize)
			}
			// Known sizes on every node (16000 MiB agent capacity, 8000 MiB
			// used): small fits the free space, huge cannot.
			for _, node := range r.nodes {
				r.tagsCache[node.URL] = &TagsCache{
					Models: []TagModel{
						{Name: small, Size: 2000 * mib},
						{Name: huge, Size: 30000 * mib},
					},
					FetchedAt: time.Now(),
				}
			}
			// Smoke check, not a revert detector: Route never reads capacity or
			// agent state, so it passes either way. The fit asserts below are
			// what discriminate.
			if picked, _, _ := r.Route("some-model", "", ""); picked == nil {
				t.Error("router refused to route")
			}
			if got := r.ModelFitsAnyHealthyNode(small, 0); got == oversize {
				t.Errorf("ModelFitsAnyHealthyNode(%s) = %v with oversize=%v, want %v", small, got, oversize, !oversize)
			}
			if got := r.ModelFitsAnyHealthyNode(huge, 0); got {
				t.Errorf("ModelFitsAnyHealthyNode(%s) = true with oversize=%v, want false (does not fit, or capacity unknown)", huge, oversize)
			}
		})
	}
}

// TestOversizeEvictForHeadroomZeroTotalIsSafe runs the same eviction twice:
// with the agent-reported capacity known the resident model is evicted to make
// room (so the loop is really exercised); with the reply oversize the capacity
// is unknown and nothing is evicted.
func TestOversizeEvictForHeadroomZeroTotalIsSafe(t *testing.T) {
	cases := []struct {
		name      string
		oversize  bool
		wantEvict int
	}{
		{"capacity known evicts", false, 1},
		{"oversize unknown capacity does not", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := oversizeRouter(t, a, "gpu-0")
			if tc.oversize {
				a.set(modeOversize)
			}
			r.pollAgentHosts()
			n := r.nodes[0]
			n.mu.Lock()
			n.LoadedModels = []ModelInfo{{Name: "resident", SizeVRAM: 4 << 30}}
			total := n.VRAMTotalMB
			n.mu.Unlock()
			if (total > 0) == tc.oversize {
				t.Fatalf("setup: VRAMTotalMB = %d with oversize=%v", total, tc.oversize)
			}
			// 15 GiB wanted on a 16000 MiB node holding 4 GiB: only evicting
			// the resident model makes room.
			if got := r.EvictForHeadroom(context.Background(), "gpu-0", "m", 15<<30); got != tc.wantEvict {
				t.Errorf("EvictForHeadroom evicted %d, want %d", got, tc.wantEvict)
			}
		})
	}
}

func TestOversizeThenPollNodeAndConcurrentPollsAreStable(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	// The agent flips between good and oversize while the agent poll, both
	// nodes' own polls, replica suggestions and routing all run at once; the
	// point is that -race stays quiet and nothing panics.
	const rounds = 6
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds*2; i++ {
			if i%2 == 0 {
				a.set(modeOversize)
			} else {
				a.set(modeGood)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < rounds; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); r.pollAgentHosts() }()
		go func() { defer wg.Done(); r.pollNode(r.nodes[0]) }()
		go func() { defer wg.Done(); r.pollNode(r.nodes[1]) }()
		go func() {
			defer wg.Done()
			r.ReplicaSuggestions()
			r.Route("some-model", "", "")
		}()
	}
	wg.Wait()
	// Settled: one oversize poll leaves both nodes unknown, one good poll
	// restores them.
	a.set(modeOversize)
	r.pollAgentHosts()
	for _, n := range r.nodes {
		n.mu.RLock()
		ok := n.AgentPresent && n.AgentTelemetryUnknown && !n.AgentStale
		n.mu.RUnlock()
		if !ok {
			t.Errorf("node %s unstable after concurrent oversize polls", n.Name)
		}
	}
	a.set(modeGood)
	r.pollAgentHosts()
	for _, n := range r.nodes {
		n.mu.RLock()
		ok := n.AgentPresent && !n.AgentTelemetryUnknown && n.VRAMSource == "agent"
		n.mu.RUnlock()
		if !ok {
			t.Errorf("node %s did not recover after the flapping settled", n.Name)
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

// TestOversizeThenAgentDisabledClearsEverything: an agent that is switched off
// after an oversize reply is simply not configured any more, so the node is
// neither present nor "telemetry unknown".
func TestOversizeThenAgentDisabledClearsEverything(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	a.set(modeOversize)
	r.pollAgentHosts()
	n.mu.RLock()
	setUp := n.AgentTelemetryUnknown && n.AgentPresent
	n.mu.RUnlock()
	if !setUp {
		t.Fatal("setup: node should be present with unknown telemetry")
	}
	r.SetMarborAgent(n.Host, false, mustPort(t, a.srv.URL), "tok", "http")
	r.pollAgentHosts()
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentTelemetryUnknown || n.AgentPresent {
		t.Errorf("unknown=%v present=%v after the agent was disabled, want false/false", n.AgentTelemetryUnknown, n.AgentPresent)
	}
}

// TestOversizeFirstPollNeverReadsBodyPrefix: the version in the prefix of an
// oversize reply is never parsed, so a node whose first poll is oversize has
// no agent version.
func TestOversizeFirstPollNeverReadsBodyPrefix(t *testing.T) {
	const prefix = `{"agent": {"version": "v9.9.9", "protocol_version": 1}}`
	body := []byte(prefix + strings.Repeat(" ", maxAgentStatusBodyBytes+1024-len(prefix)))
	r := pollOneAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}, 1)
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.AgentTelemetryUnknown || !n.AgentPresent {
		t.Fatalf("setup: unknown=%v present=%v, want true/true", n.AgentTelemetryUnknown, n.AgentPresent)
	}
	if n.AgentVersion != "" {
		t.Errorf("AgentVersion = %q after an oversize first poll, want empty (the body prefix is never parsed)", n.AgentVersion)
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
	n.mu.RLock()
	before := n.VRAMSource
	n.mu.RUnlock()
	if before != "agent" {
		t.Fatalf("setup: VRAMSource = %q before the GPU-less report, want agent", before)
	}
	tel := richTelemetry()
	tel.GPU.Devices = nil
	r.applyAgentTelemetry(n, tel)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource != "declared" || n.VRAMTotalMB != 24000 || n.VRAMUsedMB != 0 || n.Temperature != nil || n.PowerDrawW != 0 {
		t.Errorf("src=%q total=%d used=%d temp=%v power=%v, want declared/24000/0/nil/0", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, n.Temperature, n.PowerDrawW)
	}
}

// TestApplyAgentTelemetryGPULessBranchWithoutDeclaredVRAMFallsToNone is the
// twin of the test above for a node with no declared VRAM: the agent-sourced
// figure falls back to none and zero.
func TestApplyAgentTelemetryGPULessBranchWithoutDeclaredVRAMFallsToNone(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	r.pollAgentHosts()
	n.mu.RLock()
	before, cfg := n.VRAMSource, n.VRAMTotalMBConfig
	n.mu.RUnlock()
	if before != "agent" || cfg != 0 {
		t.Fatalf("setup: VRAMSource = %q, VRAMTotalMBConfig = %d, want agent/0", before, cfg)
	}
	tel := richTelemetry()
	tel.GPU.Devices = nil
	r.applyAgentTelemetry(n, tel)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMSource != "none" || n.VRAMTotalMB != 0 || n.VRAMUsedMB != 0 || n.Temperature != nil || n.PowerDrawW != 0 {
		t.Errorf("src=%q total=%d used=%d temp=%v power=%v, want none/0/0/nil/0", n.VRAMSource, n.VRAMTotalMB, n.VRAMUsedMB, n.Temperature, n.PowerDrawW)
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
// is dropped on an oversize reply. Roles cannot be influenced by evidence:
// resolveSchedulingRoles reads only each node's declared ReplicaPeers
// (placement.go, resolveSchedulingRolesAndHeads), and the evidence snapshot is
// consumed solely by replica suggestions (replica_suggestions.go). So the
// strongest real invariant is asserted instead: evidence really is present
// before and gone after, the declarations are untouched, and the roles and the
// routed head (gpu-1, deliberately not node 0) are identical.
func TestOversizeConfirmedReplicaGroupStaysResolved(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	for _, n := range r.nodes {
		n.mu.Lock()
		n.ReplicaPeers = peers("gpu-1", "gpu-0", "gpu-1")
		n.mu.Unlock()
	}
	r.pollAgentHosts()
	host := r.nodes[0].Host
	if _, ok := r.snapshotHostEvidence()[host]; !ok {
		t.Fatal("setup: host evidence should exist after a good poll")
	}
	before := r.resolveSchedulingRoles(r.Nodes())
	if before["gpu-1"] != RoleHead || before["gpu-0"] != RoleWorker {
		t.Fatalf("setup: roles %v, want gpu-1 head and gpu-0 worker", before)
	}
	a.set(modeOversize)
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()[host]; ok {
		t.Fatal("host evidence still present after an oversize poll")
	}
	after := r.resolveSchedulingRoles(r.Nodes())
	if after["gpu-1"] != RoleHead || after["gpu-0"] != RoleWorker {
		t.Errorf("roles after oversize %v, want unchanged head/worker", after)
	}
	for _, n := range r.nodes {
		n.mu.RLock()
		rp := n.ReplicaPeers
		n.mu.RUnlock()
		if rp == nil || rp.Head != "gpu-1" || len(rp.Members) != 2 {
			t.Errorf("node %s replica declaration changed: %+v", n.Name, rp)
		}
		r.pollNode(n)
		n.mu.RLock()
		present, unknown, src := n.AgentPresent, n.AgentTelemetryUnknown, n.VRAMSource
		n.mu.RUnlock()
		if !present || !unknown || src == "agent" {
			t.Errorf("node %s: present=%v unknown=%v VRAMSource=%q, want true/true/not agent", n.Name, present, unknown, src)
		}
	}
	// Smoke check, not a revert detector: the head comes from the declarations
	// alone, so this passes whether or not the oversize handling exists. The
	// state asserts above and the suggestion tests below are the discriminating
	// ones.
	if picked, _, _ := r.Route("some-model", "", ""); picked == nil || picked.Name != "gpu-1" {
		t.Errorf("Route picked %v, want the group head gpu-1", picked)
	}
}

// vllmTPTelemetry is a status report for one rank of a two-node vLLM launch
// whose agent states its topology. rtID ties the report to a node row pinned to
// that runtime ID; the master is named by the hostname the agent reports.
func vllmTPTelemetry(hostname, rtID string, rank int) marboragent.Telemetry {
	nn, mp := 2, 29500
	return marboragent.Telemetry{
		Agent:        marboragent.Agent{NodeID: "id-" + rtID, Version: "v0.24.0", ProtocolVersion: 1},
		Capabilities: []string{"status", "deployment.topology"},
		Host:         &marboragent.HostTelemetry{Hostname: hostname, Addrs: []string{"10.1.0." + fmt.Sprint(rank+1)}},
		Runtimes:     []marboragent.RuntimeInfo{{ID: rtID, Name: "vllm", Status: "running"}},
		Deployments: []marboragent.DeploymentReport{{
			Runtime: "vllm", RuntimeID: rtID,
			Topology: &marboragent.Topology{
				Launcher: LauncherVLLMMultiprocessing, NNodes: &nn, NodeRank: &rank,
				MasterAddr: "tp-host-0", MasterPort: &mp,
			},
		}},
	}
}

// suggestionMembers flattens every suggestion's members.
func suggestionMembers(s []ReplicaSuggestion) map[string]bool {
	out := map[string]bool{}
	for _, x := range s {
		for _, m := range x.Members {
			out[m] = true
		}
	}
	return out
}

// TestOversizeDropsReplicaSuggestionsAndRestoresThem: an undeclared pair on
// one host with vLLM multi-node evidence yields a suggestion after a good
// poll, none while the report is oversize (evidence dropped), and the
// suggestion is back after the next good poll.
func TestOversizeDropsReplicaSuggestionsAndRestoresThem(t *testing.T) {
	// Both rows share the agent host; the deployment reports are told apart by
	// the runtime ID each row is pinned to. One report carries both ranks.
	tel := vllmTPTelemetry("tp-host-0", "rt-a", 0)
	tel.Runtimes = append(tel.Runtimes, marboragent.RuntimeInfo{ID: "rt-b", Name: "vllm", Status: "running"})
	second := vllmTPTelemetry("tp-host-0", "rt-b", 1)
	tel.Deployments = append(tel.Deployments, second.Deployments...)
	a := newSwitchAgentWith(t, tel)
	r := oversizeRouter(t, a, "gpu-0", "gpu-1")
	for i, id := range []string{"rt-a", "rt-b"} {
		r.nodes[i].mu.Lock()
		r.nodes[i].AgentRuntimeID = id
		r.nodes[i].mu.Unlock()
	}

	r.pollAgentHosts()
	got, _ := r.ReplicaSuggestions()
	if len(got) == 0 {
		t.Fatal("setup: no replica suggestion after a good poll with multi-node evidence")
	}
	a.set(modeOversize)
	r.pollAgentHosts()
	if got, _ := r.ReplicaSuggestions(); len(got) != 0 {
		t.Errorf("%d replica suggestions while the report is oversize, want 0: %+v", len(got), got)
	}
	a.set(modeGood)
	r.pollAgentHosts()
	if got, _ := r.ReplicaSuggestions(); len(got) == 0 {
		t.Error("no replica suggestion after the agent recovered")
	}
}

// TestOversizeKeepsOtherHostReplicaSuggestion: two agent hosts each carry one
// rank of a vLLM launch. Only the oversize host's evidence is dropped; the
// other host's evidence, and the suggestion built from it, survives.
func TestOversizeKeepsOtherHostReplicaSuggestion(t *testing.T) {
	agentA := newSwitchAgentWith(t, vllmTPTelemetry("tp-host-0", "rt-a", 0))
	agentB := newSwitchAgentWith(t, vllmTPTelemetry("tp-host-1", "rt-b", 1))
	psA, psB := nodePSServer(), nodePSServer()
	t.Cleanup(psA.Close)
	t.Cleanup(psB.Close)
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psA.URL},
		{Name: "gpu-1", URL: strings.Replace(psB.URL, "127.0.0.1", "localhost", 1)},
	}, nil)
	hostA, hostB := r.nodes[0].Host, r.nodes[1].Host
	if hostA == hostB {
		t.Fatalf("setup: both nodes on host %q", hostA)
	}
	r.SetMarborAgent(hostA, true, mustPort(t, agentA.srv.URL), "tok", "http")
	r.SetMarborAgent(hostB, true, mustPort(t, agentB.srv.URL), "tok", "http")
	for i, id := range []string{"rt-a", "rt-b"} {
		r.nodes[i].mu.Lock()
		r.nodes[i].AgentRuntimeID = id
		r.nodes[i].mu.Unlock()
	}

	r.pollAgentHosts()
	before, _ := r.ReplicaSuggestions()
	if m := suggestionMembers(before); !m["gpu-0"] || !m["gpu-1"] {
		t.Fatalf("setup: suggestion members %v, want both gpu-0 and gpu-1", m)
	}
	agentA.set(modeOversize)
	r.pollAgentHosts()
	ev := r.snapshotHostEvidence()
	if _, ok := ev[hostA]; ok {
		t.Error("oversize host's evidence still present")
	}
	if _, ok := ev[hostB]; !ok {
		t.Error("the other host's evidence was dropped too")
	}
	after, coverage := r.ReplicaSuggestions()
	m := suggestionMembers(after)
	if len(after) == 0 || !m["gpu-1"] {
		t.Errorf("the other host's suggestion did not survive: %+v", after)
	}
	for _, s := range after {
		if s.Head == "gpu-0" || s.Confirmable {
			t.Errorf("a suggestion still rests on the oversize host's dropped evidence: %+v", s)
		}
	}
	for _, c := range coverage {
		if c.Node == "gpu-0" && c.State != CoverageNoAgent {
			t.Errorf("gpu-0 coverage = %q, want %q while its evidence is dropped", c.State, CoverageNoAgent)
		}
		if c.Node == "gpu-1" && !c.Detected {
			t.Errorf("gpu-1 detection lost with the other host's oversize reply: %+v", c)
		}
	}
}

// TestOversizeOnlyOversizeHostIsUnknown: with two agent hosts, one oversize and
// one good, only the oversize host's nodes lose their readings.
func TestOversizeOnlyOversizeHostIsUnknown(t *testing.T) {
	agentA, agentB := newSwitchAgent(t), newSwitchAgent(t)
	psA, psB := nodePSServer(), nodePSServer()
	t.Cleanup(psA.Close)
	t.Cleanup(psB.Close)
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psA.URL},
		{Name: "gpu-1", URL: strings.Replace(psB.URL, "127.0.0.1", "localhost", 1)},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, agentA.srv.URL), "tok", "http")
	r.SetMarborAgent(r.nodes[1].Host, true, mustPort(t, agentB.srv.URL), "tok", "http")
	r.pollAgentHosts()
	agentA.set(modeOversize)
	r.pollAgentHosts()
	oversize, good := r.nodes[0], r.nodes[1]
	oversize.mu.RLock()
	oUnknown, oSrc, oTemp := oversize.AgentTelemetryUnknown, oversize.VRAMSource, oversize.Temperature
	oversize.mu.RUnlock()
	good.mu.RLock()
	gUnknown, gSrc, gTemp := good.AgentTelemetryUnknown, good.VRAMSource, good.Temperature
	good.mu.RUnlock()
	if !oUnknown || oSrc == "agent" || oTemp != nil {
		t.Errorf("oversize host's node: unknown=%v src=%q temp=%v, want true/not agent/nil", oUnknown, oSrc, oTemp)
	}
	if gUnknown || gSrc != "agent" || gTemp == nil {
		t.Errorf("good host's node: unknown=%v src=%q temp=%v, want false/agent/non-nil", gUnknown, gSrc, gTemp)
	}
	ev := r.snapshotHostEvidence()
	if _, ok := ev[oversize.Host]; ok {
		t.Error("oversize host's evidence still present")
	}
	if _, ok := ev[good.Host]; !ok {
		t.Error("good host's evidence was dropped")
	}
}

// TestOversizeEndlessBodyIsBoundedAndKeepsNodeReachable: an agent that never
// stops writing is cut off at the cap, within the poll deadline, rather than
// read forever; the node stays reachable with unknown telemetry.
func TestOversizeEndlessBodyIsBoundedAndKeepsNodeReachable(t *testing.T) {
	a := newSwitchAgent(t)
	r := oversizeRouter(t, a, "gpu-0")
	n := r.nodes[0]
	a.set(modeEndless)
	start := time.Now()
	r.pollAgentHosts()
	elapsed := time.Since(start)
	// Margin against the 5s deadline: only fail when the poll plainly ran to it.
	if elapsed >= 4500*time.Millisecond {
		t.Errorf("poll took %v against an endless body, want well under the 5s deadline", elapsed)
	}
	n.mu.RLock()
	present, failures, unknown := n.AgentPresent, n.AgentFailures, n.AgentTelemetryUnknown
	n.mu.RUnlock()
	if !present || failures != 0 || !unknown {
		t.Errorf("present=%v failures=%d unknown=%v, want true/0/true", present, failures, unknown)
	}
	select {
	case <-a.endlessDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the endless handler was never released after the poll returned")
	}
	// The poller stopped reading at the cap; only socket buffers can absorb
	// more, so the writer stalls shortly past it.
	// Generous slack: this only needs to prove the read is not unbounded.
	const slack = 64 << 20
	if w := a.endlessWritten.Load(); w < maxAgentStatusBodyBytes+1 || w > maxAgentStatusBodyBytes+slack {
		t.Errorf("handler wrote %d bytes, want between the cap+1 (%d) and cap+%d", w, maxAgentStatusBodyBytes+1, slack)
	}
}
