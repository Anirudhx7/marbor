package nodeagent

import (
	"context"
	"errors"
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

func TestSchedulerSnapshotBeforeSeedIsUnknown(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}})
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
}

func TestSchedulerSeedPopulatesSnapshot(t *testing.T) {
	s := NewScheduler("v-test")
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
	s := newSchedulerWithBackends("v-test", fake, fakeHostCollector{telemetry: &HostTelemetry{}})
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
}

// TestSchedulerOmitsGPUOnBackendError proves a GPUCollector error (transient
// read failure, or genuinely no GPU present) results in an omitted gpu
// block, never a fabricated/zeroed one (R1).
func TestSchedulerOmitsGPUOnBackendError(t *testing.T) {
	fake := fakeGPUCollector{name: "test-vendor", available: true, err: errors.New("boom")}
	s := newSchedulerWithBackends("v-test", fake, fakeHostCollector{telemetry: &HostTelemetry{}})
	s.Seed()
	if snap := s.Snapshot(); snap.GPU != nil {
		t.Errorf("expected nil GPU on backend error, got %+v", snap.GPU)
	}
}

// TestSchedulerStartRefreshesInBackground verifies the background loop
// advances LastUpdated on its own tick, independent of any request - this is
// the behavior that replaces per-request nvidia-smi execution.
func TestSchedulerStartRefreshesInBackground(t *testing.T) {
	s := NewScheduler("v-test")
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
	s := NewScheduler("v-test")
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
