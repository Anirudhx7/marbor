package router

import (
	"context"
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
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestAttributeSoleModelVRAMDoesNotMutateSharedSlice(t *testing.T) {
	n := &NodeState{Name: "n"}
	for i := 0; i < 200; i++ {
		n.mu.Lock()
		n.LoadedModels = []ModelInfo{{Name: "m"}}
		n.VRAMUsedMB = 100
		// pollNode keeps iterating this slice after it releases the lock.
		snap := n.LoadedModels
		n.mu.Unlock()

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.mu.Lock()
			attributeSoleModelVRAM(n)
			n.mu.Unlock()
		}()
		_ = snap[0].SizeVRAM
		wg.Wait()

		if snap[0].SizeVRAM != 0 {
			t.Fatalf("a slice already handed to a reader was modified in place (size %d)", snap[0].SizeVRAM)
		}
		n.mu.RLock()
		got := n.LoadedModels[0].SizeVRAM
		n.mu.RUnlock()
		if got != 100*1024*1024 {
			t.Fatalf("node's own loaded model size = %d, want the attributed 100 MiB", got)
		}
	}
}

func TestNodeURLReadsAreGuardedByNodeLock(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "n", URL: "http://example.test:11434"},
	}, nil)
	n := r.nodes[0]

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n.mu.Lock()
			n.URL = "http://example.test:11435"
			n.mu.Unlock()
		}
	}()
	for i := 0; i < 300; i++ {
		r.pollNvidiaAll()
		r.estimateModelSizeBytes("http://example.test:11434", "m", false, 0)
	}
	close(stop)
	wg.Wait()
}

// The shared-state reads and writes in applyAgentTelemetry happen under one
// lock now. There is no deterministic assertion for the old window between
// the two locks, so this keeps the mixed read/write traffic under -race.
func TestApplyAgentTelemetryConcurrentWithSourceFlips(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "n", URL: "http://example.test:11434"},
	}, nil)
	n := r.nodes[0]
	tel := marboragent.Telemetry{
		GPU: &marboragent.GPUBlock{Vendor: "nvidia", Devices: []marboragent.GPUInfo{{VRAMTotalMB: 100, VRAMUsedMB: 10}}},
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			n.mu.Lock()
			if i%2 == 0 {
				n.VRAMSource = "nvidia"
			} else {
				n.VRAMSource = "none"
			}
			n.mu.Unlock()
		}
	}()
	for i := 0; i < 300; i++ {
		r.applyAgentTelemetry(n, tel)
	}
	close(stop)
	wg.Wait()
}

func TestPollAgentHostRecoversFromPanic(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:11434"},
	}, nil)
	n := r.nodes[0]
	r.client = nil // forces a nil dereference on the first request
	cfg := MarborAgentConfig{Enabled: true, Port: 9200, Scheme: "http"}

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("pollAgentHost must recover internally, but panic escaped: %v", rec)
			}
		}()
		r.pollAgentHost(n.Host, cfg, []*NodeState{n})
	}()

	n.mu.RLock()
	failures := n.AgentFailures
	n.mu.RUnlock()
	if failures < 1 {
		t.Errorf("AgentFailures = %d, want the panic counted as an unreachable poll", failures)
	}
}

// panicDeleteStore panics when the router drops a warm-state row, which is the
// first thing a successful unload does after the HTTP call.
type panicDeleteStore struct{ store.Store }

func (panicDeleteStore) DeleteWarmState(model, node string) error { panic("simulated store panic") }

func TestUnloadModelsRecordsPanicAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	logs := captureLog(t)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL, Runtime: "ollama"},
	}, nil)
	r.store = panicDeleteStore{}

	err := r.UnloadModels(context.Background(), "gpu-0", []string{"m1"})
	if err == nil {
		t.Fatal("UnloadModels returned nil after a panic in the unload goroutine; the failure must be reported")
	}
	n := r.nodes[0]
	n.mu.RLock()
	msg := n.UnloadErrors["m1"]
	n.mu.RUnlock()
	if !strings.Contains(msg, "panic") {
		t.Errorf("UnloadErrors[m1] = %q, want it to record the panic", msg)
	}
	out := logs.String()
	for _, want := range []string{"gpu-0", "m1", "simulated store panic", "goroutine"} {
		if !strings.Contains(out, want) {
			t.Errorf("panic log missing %q:\n%s", want, out)
		}
	}
}

// ensureHeadroomFixture builds a node with 20000 MB total, two 6 GiB models
// resident and a declared 10000 MB size for model "X", behind a server that
// counts unload calls (slowly, so concurrent callers overlap).
func ensureHeadroomFixture(t *testing.T, delay time.Duration) (*Router, *NodeState, *int32) {
	t.Helper()
	var unloads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		if req.URL.Path == "/api/generate" {
			atomic.AddInt32(&unloads, 1)
			time.Sleep(delay)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "n", URL: srv.URL, Runtime: "ollama", VRAMOverrides: map[string]int64{"X": 10000}},
	}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Healthy = true
	n.VRAMTotalMB = 20000
	n.LoadedModels = []ModelInfo{
		{Name: "A", SizeVRAM: 6 << 30},
		{Name: "B", SizeVRAM: 6 << 30},
	}
	n.mu.Unlock()
	r.RecordModelUse("n", "B")
	return r, n, &unloads
}

func TestEnsureHeadroomConcurrentCallersEvictOnce(t *testing.T) {
	r, n, unloads := ensureHeadroomFixture(t, 300*time.Millisecond)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.ensureHeadroom(context.Background(), n, "X")
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(unloads); got != 1 {
		t.Fatalf("unload calls = %d, want 1: the cooldown must be claimed before evicting so a second caller backs off", got)
	}
}

func TestEnsureHeadroomZeroEvictedLeavesNoCooldown(t *testing.T) {
	r, n, unloads := ensureHeadroomFixture(t, 0)
	r.SetPinnedModels("n", []string{"A", "B"})

	r.ensureHeadroom(context.Background(), n, "X")

	if got := atomic.LoadInt32(unloads); got != 0 {
		t.Fatalf("unload calls = %d, want 0 (everything is pinned)", got)
	}
	r.evictMu.Lock()
	_, stamped := r.lastEvictAt["n"]
	r.evictMu.Unlock()
	if stamped {
		t.Fatal("a pass that evicted nothing must not start the eviction cooldown")
	}
}
