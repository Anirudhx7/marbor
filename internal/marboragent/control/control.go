// Package control implements the ControlDriver abstraction: the agent-side
// answer to "how do I start/stop/restart the inference runtime process on
// this node's actual deployment" (P43, .local/specs/node-agent-capabilities.md
// section 5). Orthogonal to runtime detection (internal/marboragent's
// RuntimeDetector, telemetry.RuntimeInfo) - a ControlDriver never knows
// whether it's controlling Ollama, vLLM, or any other runtime, and a
// RuntimeDetector never knows whether the process it found is supervised by
// systemd, Docker, or anything else. Composed per node, never an N x M
// product of driver types.
package control

import "context"

// ControlDriver is the internal adapter for process lifecycle control. Every
// v1 driver (Systemd, Docker, Process, Launchd, WindowsService) implements
// this identically - callers never type-switch on the concrete driver.
type ControlDriver interface {
	// Name returns the driver identifier used in node config and telemetry
	// (e.g. "systemd", "docker", "process", "launchd", "windows_service").
	Name() string

	// Requires lists the privileges/tools this driver needs (e.g.
	// "systemctl", "docker.sock", "launchctl", "sc.exe"), surfaced by the UI
	// so an operator sees why an action is unavailable instead of a
	// mysterious failure.
	Requires() []string

	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Status(ctx context.Context) (Status, error)

	// Logs tails/fetches recent stdout/stderr through whatever mechanism
	// this driver's supervisor exposes (journalctl, docker logs, log show,
	// Event Viewer). Lives here, not on a runtime abstraction, because log
	// retrieval is a property of how the process is supervised, not what
	// runtime software it is. Returns a real error (never a fabricated
	// empty result - R1) when this driver has no log-retrieval mechanism.
	Logs(ctx context.Context, lines int) ([]string, error)

	// Validate confirms the configured identifier still resolves (unit
	// still exists, container still exists, PID file still present, service
	// still installed). Catches configuration drift proactively rather than
	// only surfacing as a failed Start/Stop/Restart.
	Validate(ctx context.Context) error
}

// Status is the result of a ControlDriver's live status query. Detail is the
// driver-native raw status string (e.g. "active", "running", "STOPPED") so
// an operator debugging an unexpected Running value can see exactly what the
// underlying supervisor reported, never a value invented by this package.
type Status struct {
	Running bool
	Detail  string
}
