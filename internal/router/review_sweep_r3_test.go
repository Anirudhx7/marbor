package router

import (
	"context"
	"encoding/json"
	"math"
	"net"
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
	"github.com/Anirudhx7/marbor/internal/store"
)

const gibBytes = int64(1) << 30

func TestEvictForHeadroomChargesAndCreditsTheSameLastKnownSize(t *testing.T) {
	r, _, rec := evictFixture(t, 10240, []ModelInfo{{Name: "g1"}, {Name: "g2"}})
	setNodeRuntime(r, "")
	r.recordLastKnownVRAM("n", "g1", 4*gibBytes)
	r.recordLastKnownVRAM("n", "g2", 4*gibBytes)
	// Their last-known sizes already take most of the node, so making room for
	// 9 GiB has to evict; counting them as free would skip the eviction.
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", 9*gibBytes); evicted == 0 || len(rec.got()) == 0 {
		t.Fatalf("evicted %d, unloaded %v; want an eviction because the last-known sizes are in use", evicted, rec.got())
	}
}

func TestEvictForHeadroomSkipsRuntimeThatCannotUnload(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	logs := captureLog(t)
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "v", URL: srv.URL, Runtime: "vllm"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.VRAMTotalMB = 10240
	n.LoadedModels = []ModelInfo{{Name: "a", SizeVRAM: 8 * gibBytes}}
	n.mu.Unlock()

	evicted := 0
	if r.evictForHeadroom(context.Background(), "v", "X", 9*gibBytes, &evicted) || evicted != 0 {
		t.Fatalf("evicted=%d: a runtime that cannot unload cannot make room", evicted)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("no request should be sent for a victim that cannot be unloaded")
	}
	if !strings.Contains(logs.String(), "cannot unload") {
		t.Fatalf("expected a log line saying the runtime cannot unload, got %q", logs.String())
	}
}

func TestEnsureHeadroomPanicClearsWarmReservation(t *testing.T) {
	r, n, _ := ensureHeadroomFixture(t, nil)
	r.store = panicDeleteStore{}
	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		r.ensureHeadroom(context.Background(), n, "X")
	}()
	if recovered == nil {
		t.Fatal("the injected store panic never reached ensureHeadroom, so the test proves nothing")
	}
	if got := r.PendingPrewarmBytes("n"); got != 0 {
		t.Fatalf("PendingPrewarmBytes = %d after a panicked eviction, want the reservation given back", got)
	}
}

func TestRecordModelUseIgnoresRemovedNode(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.RecordModelUse("n", "m")
	r.RemoveNode("n")
	r.RecordModelUse("n", "m")
	r.recordLastKnownVRAM("n", "m", 5)
	r.lruMu.Lock()
	used := len(r.lastUsed) + len(r.lastUsedCount)
	r.lruMu.Unlock()
	r.vramSeenMu.Lock()
	seen := len(r.lastKnownVRAM) + len(r.lastKnownCount)
	r.vramSeenMu.Unlock()
	if used != 0 || seen != 0 {
		t.Fatalf("a removed node left bookkeeping behind: last-used %d, last-known %d", used, seen)
	}
}

func TestRestoreWarmStateSkipsNodesThatNoLongerExist(t *testing.T) {
	st := openWarmTestStore(t)
	used := time.Now().Add(-time.Hour).Round(time.Second)
	for _, node := range []string{"live", "gone"} {
		if err := st.RecordWarmLoad(store.WarmStateRecord{Model: "m", Node: node, LastUsed: used, VRAMBytes: 1}); err != nil {
			t.Fatal(err)
		}
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "live", URL: "http://example.test:11434"}}, nil)
	r.SetStore(st)
	n, err := r.RestoreWarmState()
	if err != nil || n != 1 {
		t.Fatalf("RestoreWarmState = %d, %v; want only the live node's row restored", n, err)
	}
	if !r.lastUsedAt("gone", "m").IsZero() {
		t.Fatal("a row for a removed node must not be restored")
	}
	if r.lastUsedAt("live", "m").IsZero() {
		t.Fatal("the live node's row must be restored")
	}
}

func TestTruncateForLogIsUTF8SafeAndStripsControls(t *testing.T) {
	got := truncateForLog(strings.Repeat("é", 200), 11) // 2 bytes per rune
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Fatalf("the cut split a character or lost its marker: %q", got)
	}
	if len(got) > 11+3 {
		t.Fatalf("len %d exceeds the cap", len(got))
	}
	if got := truncateForLog("a\nb\x1b[31mc\r", 100); strings.ContainsAny(got, "\n\r\x1b") {
		t.Fatalf("control characters survived: %q", got)
	}
	if got, want := truncateForLog("bad\xffbyte", 100), "bad"+string(utf8.RuneError)+"byte"; got != want {
		t.Fatalf("invalid UTF-8 gave %q, want %q", got, want)
	}
	if got := truncateForLog("short", 100); got != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestRecoverAgentStepContainsPanicAndNextStepRuns(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.AgentTLSMismatch = true
	n.mu.Unlock()
	r.recoverAgentStep(n, "testing", func() { panic("boom\nforged line") })
	r.agentUnreachableSafe(n)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.AgentTLSMismatch || n.AgentFailures != 1 {
		t.Fatalf("mismatch=%v failures=%d, want both steps to have run", n.AgentTLSMismatch, n.AgentFailures)
	}
}

func TestLookupByModelNameIsDeterministic(t *testing.T) {
	m := map[string]int{"llama3:latest": 1}
	for i := 0; i < 50; i++ {
		if v, ok := lookupByModelName(m, "llama3"); !ok || v != 1 {
			t.Fatalf("bare name must find the :latest entry, got %d %v", v, ok)
		}
	}
	m["llama3"] = 2
	if v, _ := lookupByModelName(m, "llama3"); v != 2 {
		t.Fatalf("an exact key must win, got %d", v)
	}
	if v, _ := lookupByModelName(m, "llama3:latest"); v != 1 {
		t.Fatalf("an exact key must win, got %d", v)
	}
}

func TestLocalInterfaceAddrsRefreshesOnceUnderLoad(t *testing.T) {
	var calls int32
	localAddrMu.Lock()
	prevFn, prevCache := interfaceAddrs, localAddrCache
	localAddrCache, localAddrAt, localAddrRetryAt = nil, time.Time{}, time.Time{}
	interfaceAddrs = func() ([]net.Addr, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		return nil, nil
	}
	localAddrMu.Unlock()
	t.Cleanup(func() {
		localAddrMu.Lock()
		interfaceAddrs, localAddrCache = prevFn, prevCache
		localAddrAt, localAddrRetryAt = time.Time{}, time.Time{}
		localAddrMu.Unlock()
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); localInterfaceAddrs() }()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("interface lookup ran %d times for 20 simultaneous callers, want 1", got)
	}
}

func TestNodeClientsDoNotFollowRedirects(t *testing.T) {
	var targetHit int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&targetHit, 1)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL, http.StatusFound)
	}))
	defer redirector.Close()
	r := New(config.RoutingConfig{}, nil, nil)
	for name, c := range map[string]*http.Client{"router client": r.client, "node client": r.HTTPClientForNode(time.Second)} {
		resp, err := c.Get(redirector.URL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%s: status %d, want the 302 handed back unfollowed", name, resp.StatusCode)
		}
	}
	if atomic.LoadInt32(&targetHit) != 0 {
		t.Fatal("a redirect target was contacted")
	}
}

func TestSanitizeTelemetryClampsHostileValues(t *testing.T) {
	big := 1e300
	neg := -5.0
	nan := math.NaN()
	devs := make([]marboragent.GPUInfo, 100)
	devs[0] = marboragent.GPUInfo{VRAMUsedMB: -1, VRAMTotalMB: math.MaxInt64, FanPercent: &big, CorePercent: &neg, PowerWatts: &nan, Model: strings.Repeat("x", 5000)}
	tel := marboragent.Telemetry{
		Capabilities: make([]string, 1000),
		Host:         &marboragent.HostTelemetry{CPUPercent: &big, RAMTotalMB: math.MaxInt64},
		GPU:          &marboragent.GPUBlock{Devices: devs},
		Runtimes:     []marboragent.RuntimeInfo{{Engine: &marboragent.EngineState{KVCacheUsagePercent: &big}}},
	}
	sanitizeTelemetry(&tel)
	d := tel.GPU.Devices[0]
	if len(tel.Capabilities) != maxTelemetryListLen || len(tel.GPU.Devices) != maxTelemetryListLen {
		t.Errorf("lists not capped: %d %d", len(tel.Capabilities), len(tel.GPU.Devices))
	}
	if d.VRAMUsedMB != 0 || d.VRAMTotalMB != maxTelemetryMB || len(d.Model) != maxTelemetryStringBytes {
		t.Errorf("device not clamped: %+v", d)
	}
	if *d.FanPercent != 100 || *d.CorePercent != 0 || d.PowerWatts != nil {
		t.Errorf("percent/power not clamped: fan=%v core=%v power=%v", *d.FanPercent, *d.CorePercent, d.PowerWatts)
	}
	if *tel.Host.CPUPercent != 100 || tel.Host.RAMTotalMB != maxTelemetryMB {
		t.Errorf("host not clamped: %+v", tel.Host)
	}
	if *tel.Runtimes[0].Engine.KVCacheUsagePercent != 100 {
		t.Error("engine percent not clamped")
	}
}

func TestPollAgentClampsHostileVRAM(t *testing.T) {
	port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{NodeID: "node-id-1", Version: "v0.16.0"},
			GPU: &marboragent.GPUBlock{Count: 1, Devices: []marboragent.GPUInfo{
				{VRAMTotalMB: math.MaxInt64, VRAMUsedMB: math.MaxInt64},
			}},
		})
	})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "only", URL: "http://127.0.0.1:11434"}}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, port, "tok", "http")
	r.pollAgentHosts()
	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMTotalMB != maxTelemetryMB || n.VRAMUsedMB != maxTelemetryMB {
		t.Fatalf("VRAM total/used = %d/%d, want clamped to %d", n.VRAMTotalMB, n.VRAMUsedMB, maxTelemetryMB)
	}
}

func TestNvidiaReadingExpiresAfterThreePollIntervals(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	r.nvidiaPollInterval = time.Second
	r.nvidiaMu.Lock()
	r.nvidiaCache = map[int]GPUStats{0: {VRAMTotalMB: 100}}
	r.nvidiaCacheAt = time.Now().Add(-2 * time.Second)
	r.nvidiaMu.Unlock()
	if _, ok := r.nvidiaReading(0); !ok {
		t.Fatal("a reading inside three intervals must still be served")
	}
	r.nvidiaMu.Lock()
	r.nvidiaCacheAt = time.Now().Add(-4 * time.Second)
	r.nvidiaMu.Unlock()
	if _, ok := r.nvidiaReading(0); ok {
		t.Fatal("a reading older than three intervals must not be served")
	}
}

func TestAgentVRAMSumsAcrossDevices(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	n := r.nodes[0]
	r.applyAgentTelemetry(n, marboragent.Telemetry{
		Agent: marboragent.Agent{NodeID: "node-id-1", Version: "v0.16.0"},
		GPU: &marboragent.GPUBlock{Count: 2, Devices: []marboragent.GPUInfo{
			{Index: 0, VRAMTotalMB: 24000, VRAMUsedMB: 1000},
			{Index: 1, VRAMTotalMB: 24000, VRAMUsedMB: 2000},
		}},
	})
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMTotalMB != 48000 || n.VRAMUsedMB != 3000 {
		t.Fatalf("VRAM total/used = %d/%d, want 48000/3000 summed over both devices", n.VRAMTotalMB, n.VRAMUsedMB)
	}
}

func TestPrefixCacheHitRateIsUnknownWhenHitsExceedQueries(t *testing.T) {
	n := &NodeState{}
	f := func(v float64) *float64 { return &v }
	derivePrefixCacheHitRate(n, f(10), f(5))
	if got := derivePrefixCacheHitRate(n, f(20), f(40)); got != nil {
		t.Fatalf("rate = %v, want unknown when hits grow faster than queries", *got)
	}
}

func TestPendingPrewarmReportedBytesExcludesPlaceholder(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434", Runtime: "vllm"}}, nil)
	r.reserveColdStartBytes("http://example.test:11434", "n", "unsized")
	if got := r.PendingPrewarmBytes("n"); got != unknownModelReserveBytes {
		t.Fatalf("scheduling view = %d, want the placeholder counted", got)
	}
	if got := r.PendingPrewarmReportedBytes("n"); got != 0 {
		t.Fatalf("reported view = %d, want the placeholder excluded", got)
	}
	r.reserveWarmBytes("n", "sized", 3*gibBytes)
	if got := r.PendingPrewarmReportedBytes("n"); got != 3*gibBytes {
		t.Fatalf("reported view = %d, want only the sized reservation", got)
	}
}

func TestAttributedSoleModelSizeIsFlagged(t *testing.T) {
	n := &NodeState{VRAMUsedMB: 100, LoadedModels: []ModelInfo{{Name: "m"}}}
	attributeSoleModelVRAM(n)
	if !n.LoadedModels[0].sizeAttributed || n.LoadedModels[0].SizeVRAM != 100*1024*1024 {
		t.Fatalf("model = %+v, want an attributed size", n.LoadedModels[0])
	}
}

func TestEnsureHeadroomSignalsFailureWhenNothingCanBeEvicted(t *testing.T) {
	r, n, _ := ensureHeadroomFixture(t, nil)
	r.SetPinnedModels("n", []string{"A", "B"})
	if r.ensureHeadroom(context.Background(), n, "X") {
		t.Fatal("ensureHeadroom reported room although everything is pinned")
	}
	if got := r.PendingPrewarmBytes("n"); got != 0 {
		t.Fatalf("a refused warm must not keep its reservation, got %d", got)
	}
}

func TestModelFitsAnyHealthyNodeFailsClosedOnFetchError(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: url, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Healthy = true
	n.VRAMTotalMB = 24000
	n.mu.Unlock()
	if r.ModelFitsAnyHealthyNode("some-model", 0) {
		t.Fatal("a size unknown only because the model list could not be fetched must not count as fitting")
	}
}

// setNodeRuntime sets the runtime of the first node, for tests that need a
// runtime that reports no per-model VRAM size.
func setNodeRuntime(r *Router, runtime string) {
	n := r.nodes[0]
	n.mu.Lock()
	n.Runtime = runtime
	n.mu.Unlock()
}
