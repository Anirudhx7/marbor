package control

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DockerDriver controls a runtime process running as a Docker container,
// identified by its exact container name or ID. Requires docker.sock access
// - a real privilege escalation for what is otherwise a lightweight
// telemetry/action daemon (node-agent-capabilities.md section 5.4 note),
// scoped separately from Systemd/Process control at the authorization layer
// once that is built (section 7).
type DockerDriver struct {
	Container string
}

func (d *DockerDriver) Name() string       { return "docker" }
func (d *DockerDriver) Requires() []string { return []string{"docker.sock"} }

func (d *DockerDriver) Start(ctx context.Context) error   { return d.verb(ctx, "start") }
func (d *DockerDriver) Stop(ctx context.Context) error    { return d.verb(ctx, "stop") }
func (d *DockerDriver) Restart(ctx context.Context) error { return d.verb(ctx, "restart") }

func (d *DockerDriver) verb(ctx context.Context, verb string) error {
	out, err := runCommand(ctx, "docker", verb, d.Container)
	if err != nil {
		return fmt.Errorf("docker: %s %s: %s", verb, d.Container, firstNonEmptyLine(out, err))
	}
	return nil
}

func (d *DockerDriver) Status(ctx context.Context) (Status, error) {
	out, err := runCommand(ctx, "docker", "inspect", "-f", "{{.State.Running}}", d.Container)
	if err != nil {
		return Status{}, fmt.Errorf("docker: inspect %s: %s", d.Container, firstNonEmptyLine(out, err))
	}
	state := strings.TrimSpace(out)
	return Status{Running: state == "true", Detail: state}, nil
}

func (d *DockerDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	out, err := runCommand(ctx, "docker", "logs", "--tail", strconv.Itoa(lines), d.Container)
	if err != nil {
		return nil, fmt.Errorf("docker: logs %s: %s", d.Container, firstNonEmptyLine(out, err))
	}
	return splitLines(out), nil
}

// Validate confirms the container still exists (catches the exact drift
// scenario the design doc calls out: a container renamed after setup).
func (d *DockerDriver) Validate(ctx context.Context) error {
	out, err := runCommand(ctx, "docker", "inspect", d.Container)
	if err != nil {
		return fmt.Errorf("docker: container %q not found: %s", d.Container, firstNonEmptyLine(out, err))
	}
	return nil
}
