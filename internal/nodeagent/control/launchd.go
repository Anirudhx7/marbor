package control

import (
	"context"
	"fmt"
	"strings"
)

// LaunchdDriver controls a runtime process supervised by launchd (macOS,
// the MLX target's native process manager), identified by its plist label
// (e.g. "com.example.ollama"). Requires launchctl on PATH.
type LaunchdDriver struct {
	Label string
}

func (d *LaunchdDriver) Name() string       { return "launchd" }
func (d *LaunchdDriver) Requires() []string { return []string{"launchctl"} }

func (d *LaunchdDriver) Start(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "start", d.Label)
	if err != nil {
		return fmt.Errorf("launchd: start %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return nil
}

func (d *LaunchdDriver) Stop(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "stop", d.Label)
	if err != nil {
		return fmt.Errorf("launchd: stop %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return nil
}

// Restart uses `launchctl kickstart -k`, launchd's own restart primitive
// (start+stop is not equivalent for a launchd job with KeepAlive set).
func (d *LaunchdDriver) Restart(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "kickstart", "-k", "system/"+d.Label)
	if err != nil {
		return fmt.Errorf("launchd: kickstart %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return nil
}

// Status parses `launchctl list <label>`, whose output is "PID Status
// Label" on success, or a "Could not find service" style message on stderr
// when the label isn't loaded.
func (d *LaunchdDriver) Status(ctx context.Context) (Status, error) {
	out, err := runCommand(ctx, "launchctl", "list", d.Label)
	if err != nil {
		return Status{}, fmt.Errorf("launchd: list %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	trimmed := strings.TrimSpace(out)
	fields := strings.Fields(trimmed)
	running := len(fields) > 0 && fields[0] != "-"
	return Status{Running: running, Detail: trimmed}, nil
}

// Logs runs `log show` for this label and returns the last `lines` entries
// - launchd/unified logging has no direct "last N lines" flag, so the count
// contract is enforced in Go after the fact (same approach as WindowsService).
func (d *LaunchdDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	out, err := runCommand(ctx, "log", "show", "--predicate", fmt.Sprintf("subsystem == %q", d.Label), "--last", "1h")
	if err != nil {
		return nil, fmt.Errorf("launchd: log show %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return lastN(splitLines(out), lines), nil
}

// Validate confirms the label is still loaded (catches a removed/renamed
// plist).
func (d *LaunchdDriver) Validate(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "list", d.Label)
	if err != nil {
		return fmt.Errorf("launchd: label %q not loaded: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return nil
}
