package nodeagent

import (
	"encoding/json"
	"net/http"
)

// Server is the Node Agent's local HTTP server: GET /telemetry (canonical
// JSON) and GET /metrics (Prometheus text, derived from the same struct),
// both gated by an exact-match bearer token (see auth.go). Pull-only,
// polled by the mesh's existing router poll cycle - see
// .local/specs/node-agent.md section 3.
//
// Both routes serve Scheduler's cached snapshot rather than collecting on
// every request - see scheduler.go. Scheduler is normally supplied by
// agent.go's Run (seeded before the server starts, refreshed on a
// background tick). A Server built with Scheduler left nil (e.g. an older
// test) falls back to a one-off Scheduler per request, so the zero value
// stays usable rather than panicking - see snapshot() below.
//
// This type is deliberately just an HTTP router: adding a future action
// route (e.g. POST /actions/restart-runtime) needs no change here beyond
// registering it on the mux, since Server never encodes GPU/host/telemetry
// assumptions itself - those live entirely behind the Scheduler/GPUCollector/
// HostCollector seam.
type Server struct {
	Token     string
	Version   string
	Scheduler *Scheduler
}

// Handler returns the agent's http.Handler with both routes registered and
// token-gated.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /telemetry", requireToken(s.Token, s.handleTelemetry))
	mux.HandleFunc("GET /metrics", requireToken(s.Token, s.handleMetrics))
	mux.HandleFunc("POST /actions/pull_model", requireToken(s.Token, s.handlePullModel))
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

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	t := s.snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(t)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	t := s.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(RenderPrometheus(t)))
}
