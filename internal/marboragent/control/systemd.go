package control

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SystemdDriver controls a runtime process supervised by systemd, identified
// by its exact unit name (e.g. "ollama.service"). Requires systemctl on
// PATH and permission to manage the unit - exact privilege setup (root, or a
// polkit/sudo rule) is an install-time concern, not this driver's.
type SystemdDriver struct {
	Unit string
}

func (d *SystemdDriver) Name() string       { return "systemd" }
func (d *SystemdDriver) Requires() []string { return []string{"systemctl"} }

func (d *SystemdDriver) Start(ctx context.Context) error   { return d.verb(ctx, "start") }
func (d *SystemdDriver) Stop(ctx context.Context) error    { return d.verb(ctx, "stop") }
func (d *SystemdDriver) Restart(ctx context.Context) error { return d.verb(ctx, "restart") }

func (d *SystemdDriver) verb(ctx context.Context, verb string) error {
	out, err := runCommand(ctx, "systemctl", verb, d.Unit)
	if err != nil {
		return fmt.Errorf("systemd: %s %s: %s", verb, d.Unit, firstNonEmptyLine(out, err))
	}
	return nil
}

// Status runs `systemctl is-active` - this exits non-zero for every state
// other than "active" (inactive/failed/activating/...), so a non-nil error
// alone does not mean the query itself failed; the printed state is what
// matters, not the exit code.
func (d *SystemdDriver) Status(ctx context.Context) (Status, error) {
	out, err := runCommand(ctx, "systemctl", "is-active", d.Unit)
	state := strings.TrimSpace(out)
	if state == "" {
		return Status{}, fmt.Errorf("systemd: is-active %s: %s", d.Unit, firstNonEmptyLine(out, err))
	}
	return Status{Running: state == "active", Detail: state}, nil
}

func (d *SystemdDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	out, err := runCommand(ctx, "journalctl", "-u", d.Unit, "-n", strconv.Itoa(lines), "--no-pager")
	if err != nil {
		return nil, fmt.Errorf("systemd: journalctl -u %s: %s", d.Unit, firstNonEmptyLine(out, err))
	}
	return splitLines(out), nil
}

// Validate confirms the unit is still known to systemd (catches "container
// renamed six months after setup"-style drift for the systemd case: a unit
// file that was removed or never existed).
func (d *SystemdDriver) Validate(ctx context.Context) error {
	out, err := runCommand(ctx, "systemctl", "list-unit-files", d.Unit, "--no-legend")
	if err != nil {
		return fmt.Errorf("systemd: unit %q not found: %s", d.Unit, firstNonEmptyLine(out, err))
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("systemd: unit %q not found", d.Unit)
	}
	return nil
}

// firstNonEmptyLine extracts the most useful single line from a command's
// combined output for an error message (systemctl/journalctl/launchctl/sc
// diagnostics are usually one meaningful line buried in boilerplate) -
// falls back to the raw error when output carried nothing usable. err == nil
// isn't reachable via any current caller (all pass the error from a failed
// runCommand), but guarded rather than dereferenced unconditionally so a
// future caller passing a nil error can't nil-pointer-panic here.
func firstNonEmptyLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}
