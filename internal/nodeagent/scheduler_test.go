package nodeagent

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// fakeGPUCollector is a deterministic GPUCollector for tests that must not
// depend on whether the machine running them actually has an NVIDIA GPU.
type fakeGPUCollector struct {
	name      string
	available bool
	telemetry GPUBlock
	err       error
}

func (f fakeGPUCollector) Name() string                   { return f.name }
func (f fakeGPUCollector) Available(context.Context) bool { return f.available }
func (f fakeGPUCollector) Collect(context.Context) (GPUBlock, error) {
	return f.telemetry, f.err
}

// fakeHostCollector is a deterministic HostCollector for tests.
type fakeHostCollector struct {
	telemetry *HostTelemetry
}

func (f fakeHostCollector) Collect(context.Context) *HostTelemetry { return f.telemetry }

// fakeRuntimeDetector is a deterministic RuntimeDetector for tests - real
// runtime detection makes network calls to localhost:11434/8000/8080, which
// tests must not depend on (a developer's own machine may genuinely have
// Ollama running locally, which would make a test relying on "no runtime
// detected" flaky).
type fakeRuntimeDetector struct {
	name  string
	url   string
	found bool
}

func (f fakeRuntimeDetector) Detect(context.Context) (string, string, bool) {
	return f.name, f.url, f.found
}

// noRuntimeDetector is the fakeRuntimeDetector "nothing found" case, used by
// every test below that isn't specifically exercising runtime-detection
// propagation, so they stay fast and deterministic.
var noRuntimeDetector = fakeRuntimeDetector{}

func TestSchedulerSnapshotBeforeSeedIsUnknown(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	snap := s.Snapshot()
	if !snap.LastUpdated.IsZero() {
		t.Errorf("LastUpdated = %v, want zero time before Seed has run", snap.LastUpdated)
	}
	if snap.GPU != nil || snap.Host != nil || snap.Runtime != nil {
		t.Errorf("expected nil GPU/Host/Runtime before any collection, got GPU=%v Host=%v Runtime=%v", snap.GPU, snap.Host, snap.Runtime)
	}
	if snap.Agent.ProtocolVersion != ProtocolVersion || snap.Agent.Version != "v-test" {
		t.Errorf("agent.protocol_version/version should still be set: %+v", snap.Agent)
	}
	// Metadata (node_id/capabilities/platform/architecture) is known at
	// construction time, not collection time - it must be populated even
	// before the first Seed/refresh.
	if snap.Agent.NodeID == "" {
		t.Error("expected node_id to be set even before Seed")
	}
	if len(snap.Capabilities) == 0 {
		t.Error("expected capabilities to be set even before Seed")
	}
	if snap.Agent.Platform == "" || snap.Agent.Architecture == "" {
		t.Errorf("expected platform/architecture to be set even before Seed, got platform=%q architecture=%q", snap.Agent.Platform, snap.Agent.Architecture)
	}
}

func TestSchedulerSeedPopulatesSnapshot(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	snap := s.Snapshot()
	if snap.LastUpdated.IsZero() {
		t.Error("LastUpdated should be set after Seed")
	}
	if time.Since(snap.LastUpdated) > 5*time.Second {
		t.Errorf("LastUpdated = %v, expected close to now", snap.LastUpdated)
	}
}

// TestSchedulerUsesInjectedGPUBackend proves Scheduler only ever talks to the
// GPUCollector interface - a fake vendor's reading flows straight through to
// the served snapshot, including its Vendor tag, with no nvidia-smi involved
// at all. This is the concrete behavior the vendor-agnostic refactor buys:
// a real ROCm/Apple/Intel GPUCollector would be exercised identically.
func TestSchedulerUsesInjectedGPUBackend(t *testing.T) {
	fake := fakeGPUCollector{
		name:      "test-vendor",
		available: true,
		telemetry: GPUBlock{Count: 1, Devices: []GPUInfo{{Index: 0, Vendor: "test-vendor", VRAMTotalMB: 1000, VRAMUsedMB: 250}}},
	}
	s := newSchedulerWithBackends("v-test", fake, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	snap := s.Snapshot()
	if snap.GPU == nil {
		t.Fatal("expected GPU telemetry from the injected fake backend")
	}
	if snap.GPU.Vendor != "test-vendor" {
		t.Errorf("GPU.Vendor = %q, want test-vendor", snap.GPU.Vendor)
	}
	if len(snap.GPU.Devices) != 1 || snap.GPU.Devices[0].VRAMTotalMB != 1000 {
		t.Errorf("GPU.Devices = %v, want one device with VRAMTotalMB=1000", snap.GPU.Devices)
	}
}

// TestSchedulerReportsGPUVendorOnBackendError proves a GPUCollector error
// (transient read failure) still reports the gpu block with its Vendor set
// and an empty device list, never a fabricated/zeroed device reading (R1) -
// "which backend is selected" is a static fact about the process, not a live
// reading that can fail.
func TestSchedulerReportsGPUVendorOnBackendError(t *testing.T) {
	fake := fakeGPUCollector{name: "test-vendor", available: true, err: errors.New("boom")}
	s := newSchedulerWithBackends("v-test", fake, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	snap := s.Snapshot()
	if snap.GPU == nil {
		t.Fatal("expected a non-nil gpu block even on a backend collection error")
	}
	if snap.GPU.Vendor != "test-vendor" {
		t.Errorf("GPU.Vendor = %q, want test-vendor even when Collect() errors", snap.GPU.Vendor)
	}
	if len(snap.GPU.Devices) != 0 {
		t.Errorf("expected no devices on backend error, got %v", snap.GPU.Devices)
	}
}

// TestSchedulerOmitsGPUWhenNoBackend verifies a host with no GPU backend at
// all (detectGPUCollector fell back to noGPUCollector) reports no gpu block
// whatsoever - there is nothing to report, not even a vendor.
func TestSchedulerOmitsGPUWhenNoBackend(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{name: "none", available: true}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	if snap := s.Snapshot(); snap.GPU != nil {
		t.Errorf("expected nil GPU on a no-GPU host, got %+v", snap.GPU)
	}
}

// TestSchedulerMetadataFields verifies node_id/capabilities/platform/
// architecture all flow from the Scheduler's construction-time detection
// through to every served snapshot, and runtime detection populates the
// runtime resource.
func TestSchedulerMetadataFields(t *testing.T) {
	gpu := fakeGPUCollector{name: "nvidia", available: true, telemetry: GPUBlock{Vendor: "nvidia"}}
	rd := fakeRuntimeDetector{name: "ollama", url: "http://localhost:11434", found: true}
	s := newSchedulerWithBackends("v-test", gpu, fakeHostCollector{telemetry: &HostTelemetry{}}, rd)
	s.Seed()
	snap := s.Snapshot()

	if snap.Agent.NodeID == "" {
		t.Error("expected node_id to be set")
	}
	if len(snap.Capabilities) != 5 || snap.Capabilities[0] != "status" || snap.Capabilities[1] != "models.pull" || snap.Capabilities[2] != "models.list" || snap.Capabilities[3] != "models.delete" || snap.Capabilities[4] != "models.unload" {
		t.Errorf("Capabilities = %v, want [status models.pull models.list models.delete models.unload]", snap.Capabilities)
	}
	if snap.Agent.Platform != runtime.GOOS {
		t.Errorf("Platform = %q, want %q", snap.Agent.Platform, runtime.GOOS)
	}
	if snap.Agent.Architecture != runtime.GOARCH {
		t.Errorf("Architecture = %q, want %q", snap.Agent.Architecture, runtime.GOARCH)
	}
	if snap.GPU == nil || snap.GPU.Vendor != "nvidia" {
		t.Errorf("GPU.Vendor = %v, want nvidia", snap.GPU)
	}
	if snap.Runtime == nil || snap.Runtime.Name != "ollama" {
		t.Fatalf("Runtime = %v, want name=ollama", snap.Runtime)
	}
}

// TestSchedulerNodeIDStableAcrossConstructions verifies node_id is persisted
// (via identity.go) and reused across separate Scheduler constructions in
// the same environment - "still the same node" across an agent restart.
func TestSchedulerNodeIDStableAcrossConstructions(t *testing.T) {
	s1 := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s2 := newSchedulerWithBackends("v-test-2", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	if s1.nodeID == "" || s2.nodeID == "" {
		t.Fatal("expected both schedulers to have a non-empty node_id")
	}
	if s1.nodeID != s2.nodeID {
		t.Errorf("node_id changed across constructions: %q vs %q", s1.nodeID, s2.nodeID)
	}
}

// TestSchedulerOmitsRuntimeWhenNotDetected verifies "no local runtime found"
// leaves Runtime nil (and therefore omitted from JSON), never a guessed
// value (R1).
func TestSchedulerOmitsRuntimeWhenNotDetected(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	if snap := s.Snapshot(); snap.Runtime != nil {
		t.Errorf("Runtime = %v, want nil when RuntimeDetector found nothing", snap.Runtime)
	}
}

// TestSchedulerStartRefreshesInBackground verifies the background loop
// advances LastUpdated on its own tick, independent of any request - this is
// the behavior that replaces per-request nvidia-smi execution.
func TestSchedulerStartRefreshesInBackground(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	first := s.Snapshot().LastUpdated

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx, 15*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Snapshot().LastUpdated.After(first) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("LastUpdated never advanced after Start began ticking")
}

// TestSchedulerStartStopsOnContextCancel verifies the background goroutine
// actually stops refreshing once its context is canceled, rather than
// leaking a ticker that keeps mutating the cache forever.
func TestSchedulerStartStopsOnContextCancel(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Start(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // let it tick a couple times
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// TestNewSchedulerRealDetection is the one test that exercises the real
// NewScheduler constructor (real GPU/runtime detection) end to end, so the
// wiring itself (not just the fakes) is verified at least once. GPU
// detection (exec.LookPath) is local and fast; runtime detection may hit
// connection-refused on the probed localhost ports, which fails fast rather
// than waiting out the full per-probe timeout, so this stays reasonably
// quick even when nothing is listening.
func TestNewSchedulerRealDetection(t *testing.T) {
	s := NewScheduler("v-test")
	s.Seed()
	snap := s.Snapshot()
	if snap.Agent.Platform != runtime.GOOS || snap.Agent.Architecture != runtime.GOARCH {
		t.Errorf("Platform/Architecture = %q/%q, want %q/%q", snap.Agent.Platform, snap.Agent.Architecture, runtime.GOOS, runtime.GOARCH)
	}
	if snap.Agent.NodeID == "" {
		t.Error("expected node_id to always be set")
	}
}
