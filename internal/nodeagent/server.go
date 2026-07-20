package nodeagent

import (
	"encoding/json"
	"net/http"
)

// Server is the Node Agent Protocol's local HTTP server: GET /v1/status
// (canonical JSON resource envelope), GET /metrics (Prometheus text, derived
// from the same struct, left unversioned per Prometheus's own scrape-target
// convention), and POST /v1/models (the first mutating resource - pull a
// model onto this node) - all gated by an exact-match bearer token (see
// auth.go). Pull-only, polled by the mesh's existing router poll cycle - see
// .local/specs/node-agent.md section 3.
//
// GET /v1/status and GET /metrics serve Scheduler's cached snapshot rather
// than collecting on every request - see scheduler.go. Scheduler is normally
// supplied by agent.go's Run (seeded before the server starts, refreshed on
// a background tick). A Server built with Scheduler left nil (e.g. an older
// test) falls back to a one-off Scheduler per request, so the zero value
// stays usable rather than panicking - see snapshot() below.
//
// This type is deliberately just an HTTP router: adding a future resource
// route (e.g. GET /v1/models, POST /v1/runtime/restart) needs no change
// here beyond registering it on the mux, since Server never encodes
// GPU/host/runtime assumptions itself - those live entirely behind the
// Scheduler/GPUCollector/HostCollector seam.
type Server struct {
	Token     string
	Version   string
	Scheduler *Scheduler
}

// Handler returns the agent's http.Handler with every route registered and
// token-gated.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", requireToken(s.Token, s.handleStatus))
	mux.HandleFunc("GET /metrics", requireToken(s.Token, s.handleMetrics))
	mux.HandleFunc("POST /v1/models", requireToken(s.Token, s.handlePullModel))
	mux.HandleFunc("GET /v1/models", requireToken(s.Token, s.handleListModels))
	// "{name...}" (not "{name}") deliberately - model names routinely
	// contain "/" (e.g. "org/repo"), and the trailing "..." wildcard is what
	// makes ServeMux capture the rest of the path, slashes included, instead
	// of stopping at the first one.
	mux.HandleFunc("DELETE /v1/models/{name...}", requireToken(s.Token, s.handleDeleteModel))
	return mux
}

// snapshot returns the current telemetry reading: the Scheduler's cached
// value in normal operation, or one freshly-detected-and-collected reading
// (re-running GPU/host detection) if this Server has no Scheduler wired up.
// The fallback path is deliberately not optimized - it exists only so the
// zero-value Server stays usable in tests/callers that predate the caching
// change, not as a production code path.
func (s *Server) snapshot() Telemetry {
	if s.Scheduler != nil {
		return s.Scheduler.Snapshot()
	}
	sched := NewScheduler(s.Version)
	sched.Seed()
	return sched.Snapshot()
}

// runtimeTarget returns the locally-detected runtime name and the base URL
// it answered on - the same one-off-Scheduler-when-nil fallback as
// snapshot() above, since handleListModels needs the runtime's own URL, not
// just its name (which Telemetry.Runtime already exposes via snapshot()).
func (s *Server) runtimeTarget() (name, url string) {
	sched := s.Scheduler
	if sched == nil {
		sched = NewScheduler(s.Version)
		sched.Seed()
	}
	return sched.RuntimeTarget()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	t := s.snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(t)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	t := s.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(RenderPrometheus(t)))
}
