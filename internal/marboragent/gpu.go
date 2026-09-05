package marboragent

import (
	"context"
	"errors"
)

// errNoGPUBackend is returned by noGPUCollector.Collect - the explicit
// "no GPU backend on this host" signal that makes Scheduler.refresh omit
// the gpu block rather than treat a nil GPUCollector as a special case.
var errNoGPUBackend = errors.New("marboragent: no supported GPU backend detected on this host")

// GPUCollector abstracts a single GPU-vendor telemetry source: nvidia-smi
// (gpu_nvidia.go), rocm-smi (gpu_rocm.go), xpu-smi (gpu_intel.go), and
// system_profiler (gpu_apple.go) each implement it independently without
// Scheduler, Server, or the wire schema needing to change - see
// the node-agent design doc's evolution notes. Mirrors how
// internal/runtime/probe.go already lets router.go support multiple
// inference backends (Ollama/vLLM/TGI/llama.cpp) behind one interface.
type GPUCollector interface {
	// Name identifies the backend for logging and telemetry provenance
	// (e.g. "nvidia"). Surfaced on GPUBlock.Vendor so a consumer can
	// tell which backend produced a reading rather than assuming.
	Name() string
	// Available reports whether this backend's tooling is present on the
	// host at all (e.g. "nvidia-smi" resolves on PATH). Called once at
	// detection time, not on every refresh tick - selecting a GPU vendor
	// doesn't change while the process is running, same assumption
	// router.go's runtime auto-detect makes about a node's backend.
	Available(ctx context.Context) bool
	// Collect returns this vendor's full GPU reading for the host - every
	// device it found, plus any driver/CUDA-stack metadata. An error means
	// "couldn't read this cycle" - the caller reports the block with an
	// empty device list rather than fabricating a value, it never
	// means "zero everything."
	Collect(ctx context.Context) (GPUBlock, error)
}

// gpuCandidates is the ordered list of GPU backends detectGPUCollector
// tries. Add a new vendor by appending its GPUCollector implementation here
// - nothing else in this package needs to change. Order doesn't matter in
// practice: a single host's GPU vendor tooling is one of these, never more
// than one, since nvidia-smi/rocm-smi/xpu-smi/system_profiler each only
// resolve on PATH when that vendor's own driver stack installed it.
var gpuCandidates = []GPUCollector{
	nvidiaCollector{},
	rocmCollector{},
	intelCollector{},
	appleCollector{},
}

// noGPUCollector is the explicit null-object result when no candidate
// backend is available on this host (a CPU-only node, or a GPU vendor this
// agent doesn't support yet). Collect always errors so refresh() naturally
// omits the gpu block - avoids nil-GPUCollector checks scattered through the
// scheduler in favor of one typed "there is no GPU here" value (explicit
// unknown, never a guessed zero).
type noGPUCollector struct{}

func (noGPUCollector) Name() string                   { return "none" }
func (noGPUCollector) Available(context.Context) bool { return true } // the always-eligible fallback
func (noGPUCollector) Collect(context.Context) (GPUBlock, error) {
	return GPUBlock{}, errNoGPUBackend
}

// detectGPUCollector tries each candidate in order and returns the first
// one whose backend tooling is present, or noGPUCollector{} if none is.
// Called once, at Scheduler construction - not re-run on every refresh.
func detectGPUCollector(ctx context.Context) GPUCollector {
	for _, c := range gpuCandidates {
		if c.Available(ctx) {
			return c
		}
	}
	return noGPUCollector{}
}

// HostCollector abstracts the host (CPU/RAM/disk) telemetry source. Unlike
// GPUCollector, there is exactly one implementation selected per platform at
// compile time via Go build tags (host_linux.go/host_other.go) - that's
// already the correct zero-cost abstraction for "one implementation per
// GOOS," a different problem than GPU vendor selection (multiple vendors
// can exist on the very same OS, which build tags can't resolve). This
// interface exists for naming symmetry with GPUCollector and so a future
// platform-specific alternate source (e.g. a Windows WMI-backed collector)
// has a seam to slot into without changing Scheduler.
type HostCollector interface {
	Collect(ctx context.Context) *HostTelemetry
}

type stdlibHostCollector struct{}

func (stdlibHostCollector) Collect(ctx context.Context) *HostTelemetry {
	return collectHost()
}

func newHostCollector() HostCollector {
	return stdlibHostCollector{}
}
