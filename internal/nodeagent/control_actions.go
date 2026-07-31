// control_actions.go implements P43 Step 3: the agent-side handlers for
// runtime.start/runtime.stop/runtime.restart (.local/specs/
// node-agent-capabilities.md section 5). Same map-keyed-by-driver-name style
// as pullCommands/unloadCommands in actions.go, keyed by ControlDriver name
// instead of runtime name.
//
// The agent never persists control config itself (P43 Step 3 design
// decision): the mesh's Admin API constructs {driver, identifier,
// start_command} from its own store-backed router.NodeControlSetting cache
// at dispatch time and includes it in the POST body, and the agent builds
// the ControlDriver fresh per-request from exactly what the mesh tells it.
// A request with no driver configured (empty "driver" field) means the mesh
// itself has nothing configured for this node - the agent returns the exact
// error node-agent-capabilities.md section 5.6 mandates, never a guess.
//
// No self-healing (section 5.8): nothing in this file calls a ControlDriver
// method except in direct response to an incoming HTTP request.
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent/control"
)

// controlActionTimeout bounds how long the agent waits for a ControlDriver
// verb (systemctl/docker/launchctl/sc) to return - generous enough for a
// service manager that may itself wait on a graceful stop, but bounded so a
// hung subprocess can't wedge the agent's HTTP handler indefinitely.
var controlActionTimeout = 30 * time.Second

// controlActionRequest is the body POST /v1/runtime/{start,stop,restart}
// expects - constructed by the mesh's Admin API from its own store-backed
// config, never read from any agent-local state (there is none).
type controlActionRequest struct {
	Driver     string `json:"driver"`
	Identifier string `json:"identifier"`
	// StartCommand is only meaningful for the Process driver's Start action
	// - a bare PID-file convention alone gives no way to know how to launch
	// the process fresh. Ignored by every other driver's Start.
	StartCommand string `json:"start_command,omitempty"`
	// Lines is only meaningful for the runtime.logs action - how many lines
	// of log output to fetch. Ignored by start/stop/restart.
	Lines int `json:"lines,omitempty"`
}

// logsResponse is the body POST /v1/runtime/logs returns - a distinct shape
// from actionResponse since a log fetch returns data, not just ok/error.
type logsResponse struct {
	Lines []string `json:"lines,omitempty"`
	Error string   `json:"error,omitempty"`
}

func writeLogs(w http.ResponseWriter, status int, resp logsResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// defaultLogLines/maxLogLines bound an unspecified or unreasonable "lines"
// request - a snapshot fetch (R2's spirit extended to this subsystem: no
// unbounded journalctl/docker logs invocation).
const (
	defaultLogLines = 200
	maxLogLines     = 5000
)

// newControlDriver builds a control.ControlDriver from the wire request -
// a package-level var (same seam pattern as lookPath/runCommand in
// control.go) so tests can substitute a fake driver without depending on
// what's actually installed on the machine running the test.
var newControlDriver = buildControlDriver

func buildControlDriver(driver, identifier, startCommand string) (control.ControlDriver, error) {
	if startCommand != "" && driver != "process" {
		return nil, fmt.Errorf("start_command is only valid for the process driver, got %q", driver)
	}
	switch driver {
	case "systemd":
		return &control.SystemdDriver{Unit: identifier}, nil
	case "docker":
		return &control.DockerDriver{Container: identifier}, nil
	case "process":
		var cmd []string
		if startCommand != "" {
			cmd = splitCommand(startCommand)
		}
		return &control.ProcessDriver{PIDFile: identifier, StartCommand: cmd}, nil
	case "launchd":
		return &control.LaunchdDriver{Label: identifier}, nil
	case "windows_service":
		return &control.WindowsServiceDriver{Service: identifier}, nil
	default:
		return nil, fmt.Errorf("unknown control driver %q", driver)
	}
}

// splitCommand splits a start command into argv, honoring double-quoted
// segments so a path or argument containing spaces (e.g. `C:\Program
// Files\Ollama\ollama.exe`) survives as one token instead of being broken by
// a plain whitespace split. No external dependency (Architecture Law #4) -
// this is deliberately minimal, not a full shell-quoting parser.
func splitCommand(s string) []string {
	var (
		args     []string
		cur      strings.Builder
		inQuotes bool
		hasToken bool
	)
	flush := func() {
		if hasToken {
			args = append(args, cur.String())
			cur.Reset()
			hasToken = false
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			hasToken = true
		case r == ' ' && !inQuotes:
			flush()
		default:
			cur.WriteRune(r)
			hasToken = true
		}
	}
	flush()
	return args
}

// handleRuntimeStart/Stop/Restart are the POST /v1/runtime/{start,stop,
// restart} handlers, gated by the same per-node bearer token as every other
// action route (see server.go/auth.go), capabilities "runtime.start"/
// "runtime.stop"/"runtime.restart".
func (s *Server) handleRuntimeStart(w http.ResponseWriter, r *http.Request) {
	s.handleRuntimeAction(w, r, "start")
}

func (s *Server) handleRuntimeStop(w http.ResponseWriter, r *http.Request) {
	s.handleRuntimeAction(w, r, "stop")
}

func (s *Server) handleRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	s.handleRuntimeAction(w, r, "restart")
}

func (s *Server) handleRuntimeAction(w http.ResponseWriter, r *http.Request, action string) {
	var req controlActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "invalid request body"})
		return
	}

	// The agent has no persisted control config of its own - an empty
	// driver means the mesh itself has nothing configured for this node.
	// This is the exact error node-agent-capabilities.md section 5.6
	// mandates, never a guessed driver.
	if req.Driver == "" {
		writeAction(w, http.StatusUnprocessableEntity, actionResponse{Error: "Runtime control unavailable: no control driver configured"})
		return
	}

	drv, err := newControlDriver(req.Driver, req.Identifier, req.StartCommand)
	if err != nil {
		writeAction(w, http.StatusUnprocessableEntity, actionResponse{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), controlActionTimeout)
	defer cancel()

	var opErr error
	switch action {
	case "start":
		opErr = drv.Start(ctx)
	case "stop":
		opErr = drv.Stop(ctx)
	case "restart":
		opErr = drv.Restart(ctx)
	default:
		// Unreachable - action is always one of the three literals this
		// file's own handlers pass in, never derived from request input.
		opErr = fmt.Errorf("unsupported runtime action %q", action)
	}
	if opErr != nil {
		writeAction(w, http.StatusBadGateway, actionResponse{Error: opErr.Error()})
		return
	}
	writeAction(w, http.StatusOK, actionResponse{OK: true})
}

// handleRuntimeLogs is the POST /v1/runtime/logs handler, capability
// "runtime.logs". Unlike start/stop/restart this is a pure read - it never
// mutates the node - but still needs the mesh to inject driver/identifier on
// every call, same as the other three actions, since the agent holds no
// persisted control config of its own.
func (s *Server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	var req controlActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLogs(w, http.StatusBadRequest, logsResponse{Error: "invalid request body"})
		return
	}

	if req.Driver == "" {
		writeLogs(w, http.StatusUnprocessableEntity, logsResponse{Error: "Runtime control unavailable: no control driver configured"})
		return
	}

	drv, err := newControlDriver(req.Driver, req.Identifier, req.StartCommand)
	if err != nil {
		writeLogs(w, http.StatusUnprocessableEntity, logsResponse{Error: err.Error()})
		return
	}

	lines := req.Lines
	if lines <= 0 {
		lines = defaultLogLines
	} else if lines > maxLogLines {
		lines = maxLogLines
	}

	ctx, cancel := context.WithTimeout(r.Context(), controlActionTimeout)
	defer cancel()

	out, err := drv.Logs(ctx, lines)
	if err != nil {
		writeLogs(w, http.StatusBadGateway, logsResponse{Error: err.Error()})
		return
	}
	writeLogs(w, http.StatusOK, logsResponse{Lines: out})
}
