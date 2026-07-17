package nodeagent

import (
	"context"
	"runtime"
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
// GPUCollector/HostCollector/RuntimeDetector interfaces, never nvidia-smi,
// /proc, or a runtime-probe HTTP call directly. Adding a GPU vendor or a new
// host source never requires touching this file - see gpu.go/gpu_nvidia.go.
type Scheduler struct {
	version string
	gpu     GPUCollector
	host    HostCollector
	// localRuntime is the once-detected local inference runtime name (see
	// runtime_detect.go), or "" if none was found. Detected once at
	// construction, same "detect once, use it for the process lifetime"
	// shape as GPU vendor selection - a node's runtime doesn't change while
	// the agent process is running, so there's no reason to re-probe it on
	// every refresh tick (each probe attempt carries its own multi-second
	// timeout budget; repeating that every 5s for a fact that never changes
	// would be pure waste).
	localRuntime string
	snap         atomic.Pointer[Telemetry]
}

// NewScheduler creates a Scheduler for the given agent_version string,
// detecting the host's GPU backend once (nvidia-smi today; see gpu.go for
// how future vendors are added to the candidate list), selecting the
// platform's HostCollector, and detecting the local inference runtime (if
// any) once via RuntimeDetector. Call Seed once before serving requests,
// then run Start in its own goroutine to keep the cache refreshed.
func NewScheduler(version string) *Scheduler {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gpu := detectGPUCollector(ctx)
	return newSchedulerWithBackends(version, gpu, newHostCollector(), newLocalhostRuntimeDetector())
}

// newSchedulerWithBackends builds a Scheduler with explicit backends,
// bypassing detection - used by NewScheduler and by tests that need
// deterministic fakes (GPUCollector/HostCollector/RuntimeDetector) rather
// than depending on whatever hardware/local processes happen to be present
// on the machine running the test.
func newSchedulerWithBackends(version string, gpu GPUCollector, host HostCollector, rd RuntimeDetector) *Scheduler {
	// A generous but bounded budget: RuntimeDetector may try several
	// candidate ports, each with its own multi-second timeout, so this
	// needs enough room for a full worst-case "nothing is listening" sweep
	// without hanging construction indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	localRuntime, _ := rd.Detect(ctx)
	return &Scheduler{version: version, gpu: gpu, host: host, localRuntime: localRuntime}
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

// metadata returns the static agent-identity fields shared by every
// Telemetry snapshot (Seed'd or not) - capabilities/platform/architecture/
// gpu_vendor/runtime never change over the process lifetime, unlike
// GPU/Host/LastUpdated which are re-collected every refresh.
func (s *Scheduler) metadata() Telemetry {
	return Telemetry{
		SchemaVersion: SchemaVersion,
		AgentVersion:  s.version,
		Capabilities:  append([]string(nil), capabilities...),
		Platform:      runtime.GOOS,
		Architecture:  runtime.GOARCH,
		GPUVendor:     s.gpu.Name(),
		Runtime:       s.localRuntime,
	}
}

// refresh collects a fresh Telemetry snapshot from the selected GPU/host
// backends and atomically swaps it in. A GPU collection error (no GPU
// present, or a transient nvidia-smi failure) simply omits the gpu block
// (R1) - it never falls back to a stale or fabricated reading.
func (s *Scheduler) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t := s.metadata()
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
// Metadata fields (capabilities/platform/architecture/gpu_vendor/runtime)
// are already known at this point (detected during construction, not
// collection) and are populated even pre-Seed. In normal operation (Seed
// called at startup before the server accepts requests) the pre-Seed branch
// below is never observed by an HTTP client.
func (s *Scheduler) Snapshot() Telemetry {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return s.metadata()
}
