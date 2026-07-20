package nodeagent

import (
	"context"
	"net/http"
	"time"

	runtimepkg "github.com/ollama-mesh/ollama-mesh/internal/runtime"
)

// RuntimeDetector identifies which inference runtime (if any) is listening
// locally on this node, exposed as agent metadata (Telemetry.Runtime) so an
// operator debugging a mixed-version/mixed-runtime fleet can see it without
// a second hop through the mesh's own /api/ps poll. Detected once at
// Scheduler construction - same "detect once, use it for the process
// lifetime" shape as GPU vendor selection (gpu.go) - a node's runtime
// doesn't change while the agent process is running.
type RuntimeDetector interface {
	// Detect returns the runtime name ("ollama", "vllm", "tgi", "llamacpp"),
	// the base URL it answered on (needed so Scheduler can re-probe it every
	// refresh for warm models/reachability), and whether one was actually
	// found. found=false means "couldn't tell" - callers must omit the
	// runtime resource entirely (R1), never guess.
	Detect(ctx context.Context) (name string, url string, found bool)
}

// localRuntimePorts are the well-known local ports each supported runtime
// listens on by default - the same ports install.sh's own network-discovery
// wizard already probes (verify_endpoint), just aimed at localhost instead
// of scanning a subnet.
var localRuntimePorts = []string{
	"http://localhost:11434", // ollama
	"http://localhost:8000",  // vllm
	"http://localhost:8080",  // tgi / llama.cpp
}

// localhostRuntimeDetector reuses internal/runtime.DetectRuntime's existing
// signature-probing logic (the same function router.go's own auto-detect
// path calls) rather than a second, divergent copy of "how do we tell these
// four runtimes apart."
type localhostRuntimeDetector struct{ client *http.Client }

func newLocalhostRuntimeDetector() RuntimeDetector {
	return localhostRuntimeDetector{client: &http.Client{Timeout: 3 * time.Second}}
}

func (d localhostRuntimeDetector) Detect(ctx context.Context) (string, string, bool) {
	for _, url := range localRuntimePorts {
		if name, reached := runtimepkg.DetectRuntime(ctx, url, d.client); reached {
			return name, url, true
		}
	}
	return "", "", false
}
