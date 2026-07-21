package nodeagent

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	runtimepkg "github.com/ollama-mesh/ollama-mesh/internal/runtime"
)

// Scheduler runs a background refresh loop that periodically collects a
// Telemetry snapshot (via its GPUCollector, HostCollector, and the detected
// runtime's probe) into an atomic pointer, so GET /v1/status and
// GET /metrics serve a cached reading instead of forking nvidia-smi and
// re-reading host/runtime stats on every single request. Mirrors
// internal/router/health.go's nvidiaCache/pollNvidiaAll pattern - a fixed
// background tick feeding a cache that request handlers only read - applied
// to the agent side of the same GPU-stats problem.
//
// Scheduler itself is vendor/platform/runtime-agnostic: it only ever calls
// the GPUCollector/HostCollector/RuntimeDetector/internal-runtime-probe
// interfaces, never nvidia-smi, /proc, or a runtime-probe HTTP call
// directly by name. Adding a GPU vendor or a new host source never requires
// touching this file - see gpu.go/gpu_nvidia.go.
type Scheduler struct {
	version string
	nodeID  string
	gpu     GPUCollector
	host    HostCollector
	// rd is retained (not just consulted once at construction) so refresh()
	// can retry detection while localRuntime is still empty - see the
	// re-probe comment below. Never used once localRuntime is non-empty.
	rd RuntimeDetector
	// runtimeMu guards localRuntime/runtimeURL/runtimeVersion below - unlike
	// the original "written once at construction, read-only after" shape,
	// refresh() may now write these on a later tick (see localRuntime's own
	// comment), concurrently with RuntimeTarget() being read from an HTTP
	// handler goroutine.
	runtimeMu sync.RWMutex
	// localRuntime/runtimeURL are the detected local inference runtime name
	// and the base URL it answered on (see runtime_detect.go), or "" if none
	// has been found *yet*. Detection is attempted once at construction and,
	// if that finds nothing, retried on every refresh tick until it succeeds
	// - an agent process routinely starts (systemd/launchd boot ordering,
	// container restart) before its runtime is up and listening, and without
	// a retry that race would permanently omit RuntimeInfo/warm_models from
	// every /v1/status response for the rest of the agent's life, even once
	// the runtime comes up moments later. Once a runtime is found, it is
	// treated as fixed for the process lifetime (a node's runtime doesn't
	// change while the agent keeps running) and never re-probed - only its
	// live reachability/warm-models are re-probed every refresh, via
	// runtimeClient, since those genuinely change.
	localRuntime string
	runtimeURL   string
	// runtimeVersion is detected once, the same moment localRuntime is first
	// found (construction, or a later refresh-tick retry) - a runtime's own
	// reported version string cannot change while its process keeps running,
	// so re-running the version command every refresh tick would be pure
	// waste (and, for "ollama version", a forked subprocess every 5s on every
	// agent-enabled node in the fleet for an answer that's already known).
	runtimeVersion string
	runtimeClient  *http.Client
	snap           atomic.Pointer[Telemetry]
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
	name, url, _ := rd.Detect(ctx)
	var runtimeVer string
	if name != "" {
		// Bounded independently of the detection timeout above - a version
		// query is a single local command, not a multi-candidate sweep.
		vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
		runtimeVer = detectRuntimeVersion(vctx, name)
		vcancel()
	}
	return &Scheduler{
		version:        version,
		nodeID:         loadOrCreateNodeID(),
		gpu:            gpu,
		host:           host,
		rd:             rd,
		localRuntime:   name,
		runtimeURL:     url,
		runtimeVersion: runtimeVer,
		runtimeClient:  &http.Client{Timeout: 5 * time.Second},
	}
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
// Telemetry snapshot (Seed'd or not) - node_id/version/protocol_version/
// platform/architecture/capabilities never change over the process
// lifetime, unlike GPU/Host/Runtime/Health/LastUpdated which are
// re-collected every refresh.
func (s *Scheduler) metadata() Telemetry {
	return Telemetry{
		Agent: Agent{
			NodeID:          s.nodeID,
			Version:         s.version,
			ProtocolVersion: ProtocolVersion,
			Platform:        runtime.GOOS,
			Architecture:    runtime.GOARCH,
		},
		Capabilities: append([]string(nil), capabilities...),
	}
}

// refresh collects a fresh Telemetry snapshot from the selected GPU/host
// backends and the detected runtime's live probe, then atomically swaps it
// in. A GPU collection error (no GPU present, or a transient nvidia-smi
// failure) reports the gpu block with an empty device list rather than
// omitting it outright, so GPUBlock.Vendor - a static fact about which
// backend is selected, not a live reading - is still visible (R1: never
// fabricate a *reading*, but don't discard a fact that didn't fail).
func (s *Scheduler) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t := s.metadata()

	if s.gpu.Name() != "none" {
		block, err := s.gpu.Collect(ctx)
		if err != nil {
			block = GPUBlock{Vendor: s.gpu.Name()}
		} else {
			block.Vendor = s.gpu.Name()
		}
		t.GPU = &block
	}

	t.Host = s.host.Collect(ctx)

	s.runtimeMu.RLock()
	name, url, version := s.localRuntime, s.runtimeURL, s.runtimeVersion
	s.runtimeMu.RUnlock()

	// Nothing found yet - retry detection this tick rather than leaving
	// RuntimeInfo permanently omitted for the rest of the process's life (see
	// the localRuntime field comment above). Cheap once a runtime is found:
	// this branch is never reached again afterward.
	if name == "" && s.rd != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		foundName, foundURL, found := s.rd.Detect(dctx)
		dcancel()
		if found {
			vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
			foundVersion := detectRuntimeVersion(vctx, foundName)
			vcancel()

			s.runtimeMu.Lock()
			s.localRuntime, s.runtimeURL, s.runtimeVersion = foundName, foundURL, foundVersion
			s.runtimeMu.Unlock()
			name, url, version = foundName, foundURL, foundVersion
		}
	}

	if name != "" {
		ri := &RuntimeInfo{Name: name, Version: version}
		// Independent timeout, not the same ctx already spent on GPU/host
		// collection above - a slow nvidia-smi cycle must not starve this
		// probe's budget and report a false "down" purely from an expired
		// deadline it never got a fair share of.
		pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := runtimepkg.NewProbe(name, s.runtimeClient).Probe(pctx, url)
		pcancel()
		if err == nil {
			ri.Status = "up"
			for _, m := range result.LoadedModels {
				ri.WarmModels = append(ri.WarmModels, m.Name)
			}
		} else {
			ri.Status = "down"
		}
		t.Runtime = ri
		t.Health = Health{RuntimeReachable: ri.Status == "up"}
	}

	t.LastUpdated = time.Now().UTC()
	s.snap.Store(&t)
}

// Snapshot returns the most recently collected Telemetry. Before Seed has
// ever run, LastUpdated is the zero time and GPU/Host/Runtime are nil -
// callers must treat that as "not collected yet," never as a real all-zero
// reading (R1). Metadata fields (node_id/version/protocol_version/platform/
// architecture/capabilities) are already known at this point (detected
// during construction, not collection) and are populated even pre-Seed. In
// normal operation (Seed called at startup before the server accepts
// requests) the pre-Seed branch below is never observed by an HTTP client.
func (s *Scheduler) Snapshot() Telemetry {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return s.metadata()
}

// RuntimeTarget returns the detected local runtime's name and base URL - the
// same facts Telemetry.Runtime.Name is built from, exposed directly for
// callers (handleListModels) that need to dial the runtime themselves rather
// than read its already-collected telemetry. Empty until detection succeeds
// (see localRuntime's field comment - detection retries on every refresh
// tick until it does).
func (s *Scheduler) RuntimeTarget() (name, url string) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.localRuntime, s.runtimeURL
}
