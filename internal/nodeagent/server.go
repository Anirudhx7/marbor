package nodeagent

import (
	"encoding/json"
	"net/http"
	"time"
)

// Server is the Node Agent's local HTTP server: GET /telemetry (canonical
// JSON) and GET /metrics (Prometheus text, derived from the same struct),
// both gated by an exact-match bearer token (see auth.go). Pull-only,
// polled by the mesh's existing router poll cycle - see
// .local/specs/node-agent.md section 3.
//
// Both routes serve Collector's cached snapshot rather than collecting on
// every request - see collector.go. Collector is normally supplied by
// agent.go's Run (seeded before the server starts, refreshed on a
// background tick). A Server built with Collector left nil (e.g. an older
// test) falls back to collecting synchronously per request, so the zero
// value stays usable rather than panicking.
type Server struct {
	Token     string
	Version   string
	Collector *Collector
}

// Handler returns the agent's http.Handler with both routes registered and
// token-gated.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /telemetry", requireToken(s.Token, s.handleTelemetry))
	mux.HandleFunc("GET /metrics", requireToken(s.Token, s.handleMetrics))
	return mux
}

// snapshot returns the current telemetry reading: the Collector's cached
// value in normal operation, or one synchronously-collected reading if this
// Server has no Collector wired up.
func (s *Server) snapshot() Telemetry {
	if s.Collector != nil {
		return s.Collector.Snapshot()
	}
	t := Collect(s.Version)
	t.LastUpdated = time.Now().UTC()
	return t
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
