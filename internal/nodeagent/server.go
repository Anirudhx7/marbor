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
type Server struct {
	Token   string
	Version string
}

// Handler returns the agent's http.Handler with both routes registered and
// token-gated.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /telemetry", requireToken(s.Token, s.handleTelemetry))
	mux.HandleFunc("GET /metrics", requireToken(s.Token, s.handleMetrics))
	return mux
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	t := Collect(s.Version)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(t)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	t := Collect(s.Version)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(RenderPrometheus(t)))
}
