package marboragent

import (
	"context"
	"log"
	"net/http"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent/control"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
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
	// registry assigns each runtime DetectAll finds a stable RuntimeID across
	// refresh cycles - see runtime_identity.go. Only ever touched from
	// refresh()'s single goroutine (Seed, then the Start ticker), so it needs
	// no mutex of its own.
	registry *runtimeRegistry
	// runtimeMu guards primaryRuntime/primaryURL and versionCache below -
	// written by refresh() on every tick (a host-scoped agent must re-scan
	// every cycle: a second runtime can start on this host well after the
	// first was found - see DetectAll), concurrently with RuntimeTarget()
	// being read from an HTTP handler goroutine.
	runtimeMu sync.RWMutex
	// primaryRuntime/primaryURL mirror Runtimes[0] (the first entry DetectAll
	// returns this cycle) - kept as scalars for RuntimeTarget()'s single-dial
	// callers (model-pull actions), which are not yet multi-runtime aware
	// (out of scope for this change - see plan's Scope section). Empty when
	// nothing was detected this cycle.
	primaryRuntime string
	primaryURL     string
	// versionCache remembers each RuntimeID's version string once queried -
	// a runtime's own reported version cannot change while its process keeps
	// running, so re-running the version command (e.g. a forked "ollama
	// version" subprocess) every refresh tick for every runtime on every
	// agent-enabled node in the fleet would be pure waste.
	versionCache  map[string]string
	runtimeClient *http.Client
	snap          atomic.Pointer[Telemetry]
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
	// Detection itself now happens inside refresh() (called by Seed right
	// after construction, before the HTTP server starts accepting
	// connections - see agent.go) rather than here, since a host-scoped
	// agent re-scans every cycle regardless of what construction found.
	return &Scheduler{
		version:       version,
		nodeID:        loadOrCreateNodeID(),
		gpu:           gpu,
		host:          host,
		rd:            rd,
		registry:      loadOrCreateRuntimeRegistry(),
		versionCache:  make(map[string]string),
		runtimeClient: &http.Client{Timeout: 5 * time.Second},
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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("marboragent: recovered panic in refresh cycle: %v\n%s", r, debug.Stack())
		}
	}()

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

	// A host-scoped agent re-scans every candidate port every cycle (unlike
	// the old single-runtime "detect once, fixed for the process lifetime"
	// model) - a second runtime can legitimately start on this host well
	// after the first was found, and without a continuous re-scan it would
	// never be reported. The three fixed-candidate local HTTP probes this
	// does are the same class of cheap localhost dial the live-reachability
	// probe below already performs every tick for a known runtime; the
	// actually expensive operation (a forked version-query subprocess) is
	// cached per RuntimeID below, not re-run every cycle.
	var detected []DetectedRuntime
	if s.rd != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		detected = s.rd.DetectAll(dctx)
		dcancel()
	}

	if len(detected) > 0 {
		runtimes := s.registry.Reconcile(detected)
		for i := range runtimes {
			d := detected[i]

			// Independent timeout, not the same ctx already spent on
			// GPU/host collection above - a slow nvidia-smi cycle must not
			// starve this probe's budget and report a false "down" purely
			// from an expired deadline it never got a fair share of.
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
			result, err := runtimepkg.NewProbe(d.Name, s.runtimeClient).Probe(pctx, d.URL)
			pcancel()
			if err == nil {
				runtimes[i].Status = "up"
				for _, m := range result.LoadedModels {
					runtimes[i].WarmModels = append(runtimes[i].WarmModels, m.Name)
				}
			} else {
				runtimes[i].Status = "down"
			}

			runtimes[i].Version = s.runtimeVersion(runtimes[i].ID, d.Name)
		}

		t.Runtimes = runtimes
		primary := runtimes[0]
		t.Runtime = &primary
		t.Health = Health{RuntimeReachable: primary.Status == "up"}

		s.runtimeMu.Lock()
		s.primaryRuntime, s.primaryURL = detected[0].Name, detected[0].URL
		s.runtimeMu.Unlock()

		// ControlDriver discovery (P43) piggybacks the primary runtime this
		// same refresh tick - no new poll loop. Re-run every tick (a re-scan
		// is expected to reflect drift, e.g. systemd -> Docker migration,
		// freely); this only ever populates Discovered, never
		// Driver/Configured - the agent does not yet track an
		// operator-accepted driver (that arrives with the control-actions
		// capability), so those stay at their zero value here. Scoped to the
		// primary runtime only, same known limitation as RuntimeTarget (not
		// yet multi-runtime aware - see plan's Scope section).
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		disc := control.Discover(dctx, detected[0].Name, detected[0].URL)
		dcancel()
		t.Control = &ControlInfo{
			Discovered: &ControlDiscovery{
				Driver:     disc.Driver,
				Identifier: disc.Identifier,
				Evidence:   disc.Evidence,
			},
		}
	} else {
		s.runtimeMu.Lock()
		s.primaryRuntime, s.primaryURL = "", ""
		s.runtimeMu.Unlock()
	}

	t.LastUpdated = time.Now().UTC()
	s.snap.Store(&t)
}

// runtimeVersion returns id's cached version string, querying (and caching)
// it on first sight of this RuntimeID - see versionCache's field comment for
// why this must not re-run the query every cycle.
func (s *Scheduler) runtimeVersion(id, name string) string {
	s.runtimeMu.RLock()
	v, ok := s.versionCache[id]
	s.runtimeMu.RUnlock()
	if ok {
		return v
	}
	vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
	v = detectRuntimeVersion(vctx, name)
	vcancel()
	if v != "" {
		s.runtimeMu.Lock()
		s.versionCache[id] = v
		s.runtimeMu.Unlock()
	}
	return v
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

// RuntimeTarget returns the primary (first-detected this cycle) local
// runtime's name and base URL - the same facts Telemetry.Runtime is built
// from, exposed directly for callers (handleListModels, model-pull actions)
// that need to dial a runtime themselves rather than read the already-
// collected telemetry. These callers are not yet multi-runtime aware (see
// plan's Scope section) - on a host running more than one runtime, this
// always targets whichever DetectAll returned first. Empty until the first
// refresh tick completes.
func (s *Scheduler) RuntimeTarget() (name, url string) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.primaryRuntime, s.primaryURL
}
