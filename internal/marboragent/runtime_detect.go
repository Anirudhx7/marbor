package marboragent

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
)

// DetectedRuntime is one inference runtime found listening locally, before
// stable identity (RuntimeID) is assigned - see runtime_identity.go, which
// takes a []DetectedRuntime and reconciles it against the persisted registry.
type DetectedRuntime struct {
	Name string
	URL  string
	Port int
}

// RuntimeDetector identifies which inference runtime(s) (if any) are
// listening locally on this host, exposed as agent metadata
// (Telemetry.Runtimes) so an operator debugging a mixed-version/mixed-runtime
// fleet can see them without a second hop through the marbor's own /api/ps
// poll. A host-scoped agent may have more than one runtime running at once
// (e.g. Ollama on :11434 and vLLM on :8000 on the same box) - DetectAll scans
// every candidate port every cycle rather than stopping at the first hit, so
// none of them go silently unreported.
type RuntimeDetector interface {
	// Detect returns the first runtime found (name, base URL, found) - kept
	// for callers that only care about "the primary local runtime" (e.g.
	// RuntimeTarget's single-dial use case). Equivalent to the first element
	// of DetectAll, or found=false if DetectAll returns nothing.
	Detect(ctx context.Context) (name string, url string, found bool)

	// DetectAll returns every runtime currently listening on a candidate
	// port. An empty slice means "couldn't tell" - callers must omit the
	// runtime resource(s) entirely, never guess.
	DetectAll(ctx context.Context) []DetectedRuntime
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
	all := d.DetectAll(ctx)
	if len(all) == 0 {
		return "", "", false
	}
	return all[0].Name, all[0].URL, true
}

func (d localhostRuntimeDetector) DetectAll(ctx context.Context) []DetectedRuntime {
	var found []DetectedRuntime
	for _, candidate := range localRuntimePorts {
		// DetectRuntimeConfirmed, not DetectRuntime: an unidentified
		// HTTP service on a candidate port (reached=true, confirmed=false)
		// must not be permanently labeled and ID-registered as "ollama" -
		// DetectAll's own doc comment above promises "an empty slice means
		// couldn't tell," which a guessed match would violate.
		name, reached, confirmed := runtimepkg.DetectRuntimeConfirmed(ctx, candidate, d.client)
		if !reached || !confirmed {
			continue
		}
		port := 0
		if u, err := url.Parse(candidate); err == nil {
			if p, err := strconv.Atoi(u.Port()); err == nil {
				port = p
			}
		}
		found = append(found, DetectedRuntime{Name: name, URL: candidate, Port: port})
	}
	return found
}
