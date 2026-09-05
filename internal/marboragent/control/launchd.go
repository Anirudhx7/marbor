package control

import (
	"context"
	"fmt"
	"os"
	"strconv"
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

// resolveTarget probes which launchd domain d.Label is actually loaded
// under: Start/Stop previously used a bare label (caller-domain) while
// Restart hardcoded "system/<label>" - if the agent's job runs in a
// user/GUI-domain (not the system LaunchDaemon domain this product's
// documented root-run install path expects), Start/Stop would silently
// succeed against the wrong domain interpretation while Restart failed
// outright with "Could not find service." Resolving once and using the same
// domain-qualified target across all three keeps them consistent.
func (d *LaunchdDriver) resolveTarget(ctx context.Context) string {
	systemTarget := "system/" + d.Label
	if _, err := runCommand(ctx, "launchctl", "print", systemTarget); err == nil {
		return systemTarget
	}
	guiTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + d.Label
	if _, err := runCommand(ctx, "launchctl", "print", guiTarget); err == nil {
		return guiTarget
	}
	// Neither probe succeeded (permission issue, or the label genuinely
	// isn't loaded under either domain) - fall back to the system domain,
	// preserving the previous default so the resulting error message is no
	// less informative than before.
	return systemTarget
}

func (d *LaunchdDriver) Start(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "start", d.resolveTarget(ctx))
	if err != nil {
		return fmt.Errorf("launchd: start %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return nil
}

func (d *LaunchdDriver) Stop(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "stop", d.resolveTarget(ctx))
	if err != nil {
		return fmt.Errorf("launchd: stop %s: %s", d.Label, firstNonEmptyLine(out, err))
	}
	return nil
}

// Restart uses `launchctl kickstart -k`, launchd's own restart primitive
// (start+stop is not equivalent for a launchd job with KeepAlive set).
func (d *LaunchdDriver) Restart(ctx context.Context) error {
	out, err := runCommand(ctx, "launchctl", "kickstart", "-k", d.resolveTarget(ctx))
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

	// Modern macOS often emits a dict-style block (starting with "{") rather
	// than the legacy three-column summary line - the column heuristic below
	// never sees a leading "-" in that form, so it would report Running=true
	// even for a loaded-but-crashed job with no PID. Look for a `"PID" = n;`
	// pair first, matching parseLaunchctlListStatus in service_darwin.go.
	if strings.HasPrefix(trimmed, "{") {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, `"PID"`) {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				pidStr := strings.TrimSpace(strings.Trim(strings.TrimSpace(parts[1]), ";"))
				if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
					return Status{Running: true, Detail: trimmed}, nil
				}
			}
		}
		return Status{Running: false, Detail: trimmed}, nil
	}

	fields := strings.Fields(trimmed)
	running := len(fields) > 0 && fields[0] != "-"
	return Status{Running: running, Detail: trimmed}, nil
}

// Logs runs `log show` for this label and returns the last `lines` entries
// - launchd/unified logging has no direct "last N lines" flag, so the count
// contract is enforced in Go after the fact (same approach as WindowsService).
//
// The predicate matches on process name (derived from d.Label's last
// dot-separated component, e.g. "ollama" from "com.example.ollama") in
// addition to the label-based subsystem: third-party runtime binaries
// generally don't set an os_log subsystem matching their own launchd label,
// so a subsystem-only predicate returned empty for the exact target
// population this exists to surface.
func (d *LaunchdDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	processName := d.Label
	if idx := strings.LastIndex(d.Label, "."); idx != -1 && idx+1 < len(d.Label) {
		processName = d.Label[idx+1:]
	}
	predicate := fmt.Sprintf("process == %q OR subsystem == %q", processName, d.Label)
	out, err := runCommand(ctx, "log", "show", "--predicate", predicate, "--last", "1h")
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
