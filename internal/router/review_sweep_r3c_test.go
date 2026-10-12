package router

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// A run that unloads what it can and skips a pinned model is a success with a
// note, not a failed run.
func TestFireScheduleUnloadWithPinnedSkipIsOkWithWarning(t *testing.T) {
	rec := &unloadRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.SetPinnedModels("n", []string{"pinned"})
	r.schedules = []Schedule{{ID: "s1", Action: "unload", Node: "n", Models: []string{"m", "pinned"}}}
	r.fireSchedule(context.Background(), r.schedules[0])
	got := r.schedules[0]
	if got.LastStatus != "ok" {
		t.Fatalf("LastStatus = %q (%s), want ok", got.LastStatus, got.LastError)
	}
	if !strings.Contains(got.LastError, "pinned") {
		t.Fatalf("LastError = %q, want the pinned skip noted", got.LastError)
	}
	if u := rec.got(); len(u) != 1 || u[0] != "m" {
		t.Fatalf("unloaded %v, want only m", u)
	}
}

func TestFireScheduleUnloadFailureStillReadsAsError(t *testing.T) {
	rec := &unloadRecorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.schedules = []Schedule{{ID: "s1", Action: "unload", Node: "n", Models: []string{"m"}}}
	r.fireSchedule(context.Background(), r.schedules[0])
	if r.schedules[0].LastStatus != "error" {
		t.Fatalf("LastStatus = %q, want error", r.schedules[0].LastStatus)
	}
}

func redirectingServer(t *testing.T) (redirector *httptest.Server, targetHits *int32) {
	t.Helper()
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(target.Close)
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL, http.StatusFound)
	}))
	t.Cleanup(red.Close)
	return red, &hits
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	red, hits := redirectingServer(t)
	logs := &syncLog{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	r := newTestRouter(nil, config.WebhookConfig{Enabled: true, URL: red.URL})
	r.fireWebhook("node_down", "n", "http://example.test")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "returned 302") {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("the webhook followed a redirect")
	}
	if !strings.Contains(logs.String(), "returned 302") {
		t.Fatalf("a 3xx reply must be logged as not delivered, got %q", logs.String())
	}
}

func TestPingEndpointTreatsRedirectAsFailure(t *testing.T) {
	red, hits := redirectingServer(t)
	r := New(config.RoutingConfig{}, nil, nil)
	err := r.pingEndpoint(context.Background(), red.URL, "/api/generate", map[string]any{"model": "m"})
	if err == nil {
		t.Fatal("a 3xx reply to a load request must be a failure")
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("the load request followed a redirect")
	}
}

func TestRuntimeErrorDetailIsCleaned(t *testing.T) {
	body := `{"error": "bad\nforged line ` + bidiOverride + ` reversed ` + strings.Repeat("x", 600) + `"}`
	got := runtimeErrorDetail(strings.NewReader(body))
	if strings.ContainsAny(got, "\n"+bidiOverride) {
		t.Fatalf("unsafe characters survived: %q", got)
	}
	if len(got) > maxLogValueBytes+3 {
		t.Fatalf("detail is %d bytes, want it bounded", len(got))
	}
}

func TestLogCutHandlesBidiAndKeepsBound(t *testing.T) {
	got := truncateForLog("a"+bidiOverride+"b"+lineSep+"c"+isolate+"d", 100)
	if strings.ContainsAny(got, bidiOverride+lineSep+isolate) {
		t.Fatalf("bidi or separator characters survived: %q", got)
	}
	long := truncateForLog(strings.Repeat("é", 500), 20)
	if !utf8.ValidString(long) || len(long) > 20+3 {
		t.Fatalf("bad cut: %d bytes valid=%v", len(long), utf8.ValidString(long))
	}
}

func TestPrefixCacheHitRateEqualDeltasAndFirstCall(t *testing.T) {
	n := &NodeState{}
	f := func(v float64) *float64 { return &v }
	if got := derivePrefixCacheHitRate(n, f(10), f(5)); got != nil {
		t.Fatalf("first call returned %v, want unknown (no baseline yet)", *got)
	}
	got := derivePrefixCacheHitRate(n, f(20), f(15))
	if got == nil || *got != 100 {
		t.Fatalf("equal deltas gave %v, want exactly 100", got)
	}
}

// node_id ships with the /v1/status endpoint (since v0.17.1), so every agent
// that can be polled sends it. A reply without one is not a healthy agent, even
// when it carries other agent signals.
func TestPollAgentReplyWithoutNodeIDIsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"no node id":    `{"agent": {"version": "v0.17.1", "protocol_version": 1}}`,
		"blank node id": `{"agent": {"node_id": "   ", "version": "v0.17.1"}}`,
	} {
		body := body
		port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(body)) })
		r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://127.0.0.1:11434"}}, nil)
		r.SetMarborAgent(r.nodes[0].Host, true, port, "tok", "http")
		r.pollAgentHosts()
		r.nodes[0].mu.RLock()
		present, failures := r.nodes[0].AgentPresent, r.nodes[0].AgentFailures
		r.nodes[0].mu.RUnlock()
		if present || failures != 1 {
			t.Errorf("%s: present=%v failures=%d, want an unhealthy agent", name, present, failures)
		}
	}
}

func TestPollAgentRejectsNullAndEmptyReplies(t *testing.T) {
	for name, body := range map[string]string{"empty object": `{}`, "null": `null`, "unrelated service": `{"status":"ok"}`} {
		body := body
		port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(body)) })
		r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://127.0.0.1:11434"}}, nil)
		r.SetMarborAgent(r.nodes[0].Host, true, port, "tok", "http")
		r.pollAgentHosts()
		r.nodes[0].mu.RLock()
		present, failures := r.nodes[0].AgentPresent, r.nodes[0].AgentFailures
		r.nodes[0].mu.RUnlock()
		if present || failures != 1 {
			t.Errorf("%s: present=%v failures=%d, want an unhealthy agent", name, present, failures)
		}
	}
}

func TestSanitizeTelemetryCoversEveryBlock(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	big := 1e300
	negInt := -7
	hugeInt := 1 << 40
	port := 99999
	devs := make([]marboragent.GPUInfo, 70)
	devs[0] = marboragent.GPUInfo{Index: -3, CorePercent: &nan, FanPercent: &inf, VRAMUsedMB: math.MinInt64}
	tel := marboragent.Telemetry{
		Agent: marboragent.Agent{
			NodeID: strings.Repeat("n", 999), Version: "v1\x1b[31m", Platform: "   ", ProtocolVersion: -4,
		},
		Host: &marboragent.HostTelemetry{
			Hostname: "host" + bidiOverride + "name", UptimeSeconds: -1, BootTime: -2, DiskFreeGB: big, DiskTotalGB: inf,
		},
		GPU: &marboragent.GPUBlock{Count: 0, Devices: devs},
		Runtimes: []marboragent.RuntimeInfo{{
			Port: port, QueueDepth: negInt,
			Engine: &marboragent.EngineState{
				RunningRequests: &negInt, WaitingRequests: &hugeInt,
				PrefixCacheQueries: &nan, PrefixCacheHits: &inf,
			},
		}},
		Control: &marboragent.ControlInfo{
			Driver:     strings.Repeat("d", 999),
			Discovered: &marboragent.ControlDiscovery{Identifier: "id\n", Evidence: make([]string, 200)},
		},
		Deployments: []marboragent.DeploymentReport{{
			Runtime: "vllm", Port: port, GPUGroup: []int{0, -1, 5000, 3},
			GPUScope:    &marboragent.GPUScope{Indices: []int{2, -9, 70000}, Raw: strings.Repeat("r", 999)},
			Parallelism: &marboragent.ParallelismInfo{Width: hugeInt, PipelineWidth: negInt},
			Caps:        &marboragent.RuntimeCaps{MaxTPWidth: hugeInt},
			Topology: &marboragent.Topology{
				Launcher: strings.Repeat("l", 999), NNodes: &negInt, NodeRank: &hugeInt, MasterPort: &port,
				RPCServers: make([]string, 200), Evidence: make([]string, 200),
			},
		}},
	}
	sanitizeTelemetry(&tel)
	if len(tel.Agent.NodeID) != maxTelemetryStringBytes || tel.Agent.Version != "v1[31m" || tel.Agent.Platform != "" || tel.Agent.ProtocolVersion != 0 {
		t.Errorf("agent block not cleaned: %+v", tel.Agent)
	}
	if tel.Host.Hostname != "hostname" || tel.Host.UptimeSeconds != 0 || tel.Host.BootTime != 0 ||
		tel.Host.DiskFreeGB != maxTelemetryDiskGB || tel.Host.DiskTotalGB != 0 {
		t.Errorf("host block not cleaned: %+v", tel.Host)
	}
	g := tel.GPU
	if len(g.Devices) != maxTelemetryListLen || g.Count != maxTelemetryListLen {
		t.Errorf("gpu list/count: %d devices, count %d", len(g.Devices), g.Count)
	}
	if d := g.Devices[0]; d.Index != 0 || d.CorePercent != nil || d.FanPercent != nil || d.VRAMUsedMB != 0 {
		t.Errorf("device 0 not cleaned: %+v", d)
	}
	rt := tel.Runtimes[0]
	if rt.Port != 0 || rt.QueueDepth != 0 || rt.Engine.RunningRequests != nil || rt.Engine.WaitingRequests != nil ||
		rt.Engine.PrefixCacheQueries != nil || rt.Engine.PrefixCacheHits != nil {
		t.Errorf("runtime block not cleaned: %+v engine %+v", rt, rt.Engine)
	}
	if len(tel.Control.Driver) != maxTelemetryStringBytes || tel.Control.Discovered.Identifier != "id" ||
		len(tel.Control.Discovered.Evidence) != maxTelemetryListLen {
		t.Errorf("control block not cleaned: %+v", tel.Control)
	}
	d := tel.Deployments[0]
	if d.Port != 0 || d.GPUGroup != nil || d.GPUScope.Indices != nil || len(d.GPUScope.Raw) != maxTelemetryStringBytes {
		t.Errorf("deployment not cleaned: %+v scope %+v", d, d.GPUScope)
	}
	if d.Parallelism.Width != maxTelemetryCount || d.Parallelism.PipelineWidth != 0 || d.Caps.MaxTPWidth != maxTelemetryCount {
		t.Errorf("parallelism/caps not cleaned: %+v %+v", d.Parallelism, d.Caps)
	}
	tp := d.Topology
	if len(tp.Launcher) != maxTelemetryStringBytes || tp.NNodes != nil || tp.NodeRank != nil || tp.MasterPort != nil ||
		len(tp.RPCServers) != maxTelemetryListLen || len(tp.Evidence) != maxTelemetryListLen {
		t.Errorf("topology not cleaned: %+v", tp)
	}
}

func TestSanitizeGPUCountFollowsDevices(t *testing.T) {
	tel := marboragent.Telemetry{GPU: &marboragent.GPUBlock{Count: 0, Devices: make([]marboragent.GPUInfo, 3)}}
	sanitizeTelemetry(&tel)
	if tel.GPU.Count != 3 {
		t.Fatalf("Count = %d, want 3 to match the devices", tel.GPU.Count)
	}
	failed := marboragent.Telemetry{GPU: &marboragent.GPUBlock{Count: 2}}
	sanitizeTelemetry(&failed)
	if failed.GPU.Count != 2 {
		t.Fatalf("Count = %d, want the agent own count kept when collection failed", failed.GPU.Count)
	}
}

// 64 devices at the cap each must not overflow once summed and converted to bytes.
func TestPollAgentSumOfMaxDevicesStaysSafe(t *testing.T) {
	devs := make([]marboragent.GPUInfo, 64)
	for i := range devs {
		devs[i] = marboragent.GPUInfo{Index: i, VRAMTotalMB: math.MaxInt64, VRAMUsedMB: math.MaxInt64}
	}
	port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{NodeID: "node-id-1", Version: "v1"},
			GPU:   &marboragent.GPUBlock{Count: 64, Devices: devs},
		})
	})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "only", URL: "http://127.0.0.1:11434"}}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, port, "tok", "http")
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMTotalMB != maxTelemetryMB || n.VRAMUsedMB > n.VRAMTotalMB {
		t.Fatalf("total/used = %d/%d, want the sum capped at %d with used <= total", n.VRAMTotalMB, n.VRAMUsedMB, maxTelemetryMB)
	}
	if bytes := n.VRAMTotalMB * 1024 * 1024; bytes <= 0 {
		t.Fatalf("byte figure overflowed: %d", bytes)
	}
}

func TestPollAgentStoresOnlyCleanedStrings(t *testing.T) {
	port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{NodeID: "n1", Version: "v1" + bidiOverride},
			Host:  &marboragent.HostTelemetry{Hostname: "evil\nhost"},
		})
	})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://127.0.0.1:11434"}}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, port, "tok", "http")
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentVersion != "v1" || n.Hostname != "evilhost" {
		t.Fatalf("stored version %q hostname %q, want cleaned values", n.AgentVersion, n.Hostname)
	}
}

// syncLog is a log sink that is safe to read while another goroutine writes.
type syncLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Characters that must never survive into stored or logged text, built from code
// points so the source holds no invisible characters.
var (
	bidiOverride = string(rune(0x202e))
	lineSep      = string(rune(0x2028))
	isolate      = string(rune(0x2066))
)
