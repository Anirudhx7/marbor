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
	telemetry GPUTelemetry
	err       error
}

func (f fakeGPUCollector) Name() string                   { return f.name }
func (f fakeGPUCollector) Available(context.Context) bool { return f.available }
func (f fakeGPUCollector) Collect(context.Context) (GPUTelemetry, error) {
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
	found bool
}

func (f fakeRuntimeDetector) Detect(context.Context) (string, bool) { return f.name, f.found }

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
	if snap.GPU != nil || snap.Host != nil {
		t.Errorf("expected nil GPU/Host before any collection, got GPU=%v Host=%v", snap.GPU, snap.Host)
	}
	if snap.SchemaVersion != SchemaVersion || snap.AgentVersion != "v-test" {
		t.Errorf("schema_version/agent_version should still be set: %+v", snap)
	}
	// Metadata (capabilities/platform/architecture/gpu_vendor) is known at
	// construction time, not collection time - it must be populated even
	// before the first Seed/refresh.
	if len(snap.Capabilities) == 0 {
		t.Error("expected capabilities to be set even before Seed")
	}
	if snap.Platform == "" || snap.Architecture == "" {
		t.Errorf("expected platform/architecture to be set even before Seed, got platform=%q architecture=%q", snap.Platform, snap.Architecture)
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
		telemetry: GPUTelemetry{Vendor: "test-vendor", VRAMTotalMB: 1000, VRAMUsedMB: 250},
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
	if snap.GPU.VRAMTotalMB != 1000 {
		t.Errorf("GPU.VRAMTotalMB = %d, want 1000", snap.GPU.VRAMTotalMB)
	}
	// GPUVendor (top-level agent metadata) reflects the selected backend's
	// Name() unconditionally - independent of the per-cycle GPU reading.
	if snap.GPUVendor != "test-vendor" {
		t.Errorf("GPUVendor = %q, want test-vendor", snap.GPUVendor)
	}
}

// TestSchedulerOmitsGPUOnBackendError proves a GPUCollector error (transient
// read failure, or genuinely no GPU present) results in an omitted gpu
// block, never a fabricated/zeroed one (R1) - but GPUVendor metadata still
// reports which backend is selected, since that's a static fact about the
// process, not a live reading that can fail.
func TestSchedulerOmitsGPUOnBackendError(t *testing.T) {
	fake := fakeGPUCollector{name: "test-vendor", available: true, err: errors.New("boom")}
	s := newSchedulerWithBackends("v-test", fake, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	snap := s.Snapshot()
	if snap.GPU != nil {
		t.Errorf("expected nil GPU on backend error, got %+v", snap.GPU)
	}
	if snap.GPUVendor != "test-vendor" {
		t.Errorf("GPUVendor = %q, want test-vendor even when Collect() errors", snap.GPUVendor)
	}
}

// TestSchedulerMetadataFields verifies capabilities/platform/architecture/
// gpu_vendor/runtime all flow from the Scheduler's construction-time
// detection through to every served snapshot.
func TestSchedulerMetadataFields(t *testing.T) {
	gpu := fakeGPUCollector{name: "nvidia", available: true, telemetry: GPUTelemetry{Vendor: "nvidia"}}
	rd := fakeRuntimeDetector{name: "ollama", found: true}
	s := newSchedulerWithBackends("v-test", gpu, fakeHostCollector{telemetry: &HostTelemetry{}}, rd)
	s.Seed()
	snap := s.Snapshot()

	if len(snap.Capabilities) != 2 || snap.Capabilities[0] != "telemetry" || snap.Capabilities[1] != "actions.pull_model" {
		t.Errorf("Capabilities = %v, want [telemetry actions.pull_model]", snap.Capabilities)
	}
	if snap.Platform != runtime.GOOS {
		t.Errorf("Platform = %q, want %q", snap.Platform, runtime.GOOS)
	}
	if snap.Architecture != runtime.GOARCH {
		t.Errorf("Architecture = %q, want %q", snap.Architecture, runtime.GOARCH)
	}
	if snap.GPUVendor != "nvidia" {
		t.Errorf("GPUVendor = %q, want nvidia", snap.GPUVendor)
	}
	if snap.Runtime != "ollama" {
		t.Errorf("Runtime = %q, want ollama", snap.Runtime)
	}
}

// TestSchedulerOmitsRuntimeWhenNotDetected verifies "no local runtime found"
// leaves Runtime empty (and therefore omitted from JSON via omitempty),
// never a guessed value (R1).
func TestSchedulerOmitsRuntimeWhenNotDetected(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	s.Seed()
	if snap := s.Snapshot(); snap.Runtime != "" {
		t.Errorf("Runtime = %q, want empty when RuntimeDetector found nothing", snap.Runtime)
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
	if snap.Platform != runtime.GOOS || snap.Architecture != runtime.GOARCH {
		t.Errorf("Platform/Architecture = %q/%q, want %q/%q", snap.Platform, snap.Architecture, runtime.GOOS, runtime.GOARCH)
	}
	if snap.GPUVendor == "" {
		t.Error("expected GPUVendor to always be set (nvidia, or none)")
	}
}
