package nodeagent

import (
	"context"
	"sync/atomic"
	"time"
)

// Scheduler runs a background refresh loop that periodically collects a
// Telemetry snapshot (via its GPUCollector and HostCollector) into an atomic
// pointer, so GET /telemetry and GET /metrics serve a cached reading instead
// of forking nvidia-smi and re-reading host stats on every single request.
// Mirrors internal/router/health.go's nvidiaCache/pollNvidiaAll pattern - a
// fixed background tick feeding a cache that request handlers only read -
// applied to the agent side of the same GPU-stats problem.
//
// Scheduler itself is vendor/platform-agnostic: it only ever calls the
// GPUCollector/HostCollector interfaces, never nvidia-smi or /proc directly.
// Adding a GPU vendor or a new host source never requires touching this
// file - see gpu.go/gpu_nvidia.go.
type Scheduler struct {
	version string
	gpu     GPUCollector
	host    HostCollector
	snap    atomic.Pointer[Telemetry]
}

// NewScheduler creates a Scheduler for the given agent_version string,
// detecting the host's GPU backend once (nvidia-smi today; see gpu.go for
// how future vendors are added to the candidate list) and selecting the
// platform's HostCollector. Call Seed once before serving requests, then run
// Start in its own goroutine to keep the cache refreshed.
func NewScheduler(version string) *Scheduler {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return newSchedulerWithBackends(version, detectGPUCollector(ctx), newHostCollector())
}

// newSchedulerWithBackends builds a Scheduler with explicit backends,
// bypassing detection - used by NewScheduler and by tests that need a
// deterministic fake GPUCollector/HostCollector rather than depending on
// whatever hardware happens to be present on the machine running the test.
func newSchedulerWithBackends(version string, gpu GPUCollector, host HostCollector) *Scheduler {
	return &Scheduler{version: version, gpu: gpu, host: host}
}

// Seed collects one snapshot synchronously and stores it. Intended to run
// once at startup, before the HTTP server begins accepting connections, so
// the first request never observes an empty/never-collected cache.
func (s *Scheduler) Seed() {
	s.refresh()
}

// Start blocks, refreshing the cached snapshot every interval until ctx is
// canceled. Run it in its own goroutine (go scheduler.Start(ctx, interval)).
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh()
		}
	}
}

// refresh collects a fresh Telemetry snapshot from the selected GPU/host
// backends and atomically swaps it in. A GPU collection error (no GPU
// present, or a transient nvidia-smi failure) simply omits the gpu block
// (R1) - it never falls back to a stale or fabricated reading.
func (s *Scheduler) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t := Telemetry{SchemaVersion: SchemaVersion, AgentVersion: s.version}
	if gpu, err := s.gpu.Collect(ctx); err == nil {
		t.GPU = &gpu
	}
	t.Host = s.host.Collect(ctx)
	t.LastUpdated = time.Now().UTC()
	s.snap.Store(&t)
}

// Snapshot returns the most recently collected Telemetry. Before Seed has
// ever run, LastUpdated is the zero time and GPU/Host are nil - callers must
// treat that as "not collected yet," never as a real all-zero reading (R1).
// In normal operation (Seed called at startup before the server accepts
// requests) this branch is never observed by an HTTP client.
func (s *Scheduler) Snapshot() Telemetry {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return Telemetry{SchemaVersion: SchemaVersion, AgentVersion: s.version}
}
