package control

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// WindowsServiceDriver controls a runtime process registered as a Windows
// service, identified by its exact service name. Requires sc.exe (SCM
// query/control access).
type WindowsServiceDriver struct {
	Service string
}

func (d *WindowsServiceDriver) Name() string       { return "windows_service" }
func (d *WindowsServiceDriver) Requires() []string { return []string{"sc.exe"} }

func (d *WindowsServiceDriver) Start(ctx context.Context) error {
	out, err := runCommand(ctx, "sc", "start", d.Service)
	if err != nil {
		return fmt.Errorf("windows_service: start %s: %s", d.Service, firstNonEmptyLine(out, err))
	}
	return nil
}

func (d *WindowsServiceDriver) Stop(ctx context.Context) error {
	out, err := runCommand(ctx, "sc", "stop", d.Service)
	if err != nil {
		return fmt.Errorf("windows_service: stop %s: %s", d.Service, firstNonEmptyLine(out, err))
	}
	return nil
}

// Restart: sc.exe has no atomic restart verb, so this is a sequential
// stop-then-start, same tradeoff documented up front rather than silently.
func (d *WindowsServiceDriver) Restart(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	return d.Start(ctx)
}

// Status parses `sc query <service>`, which prints a "STATE" line like
// "4  RUNNING" or "1  STOPPED".
func (d *WindowsServiceDriver) Status(ctx context.Context) (Status, error) {
	out, err := runCommand(ctx, "sc", "query", d.Service)
	if err != nil {
		return Status{}, fmt.Errorf("windows_service: query %s: %s", d.Service, firstNonEmptyLine(out, err))
	}
	for _, line := range strings.Split(out, "\n") {
		if idx := strings.Index(line, "STATE"); idx != -1 {
			detail := strings.TrimSpace(line[idx:])
			return Status{Running: strings.Contains(detail, "RUNNING"), Detail: detail}, nil
		}
	}
	return Status{}, fmt.Errorf("windows_service: query %s: no STATE line in output", d.Service)
}

// Logs queries the Application event log for entries from this service's
// provider - Windows equivalent of journalctl/docker logs. Truncated to
// `lines` in Go since wevtutil's /c: flag counts events, not lines of text.
func (d *WindowsServiceDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	query := fmt.Sprintf(`*[System[Provider[@Name='%s']]]`, d.Service)
	out, err := runCommand(ctx, "wevtutil", "qe", "Application", "/q:"+query, "/c:"+strconv.Itoa(lines), "/rd:true", "/f:text")
	if err != nil {
		return nil, fmt.Errorf("windows_service: wevtutil %s: %s", d.Service, firstNonEmptyLine(out, err))
	}
	return lastN(splitLines(out), lines), nil
}

// Validate confirms the service is still installed (catches an uninstalled
// or renamed service).
func (d *WindowsServiceDriver) Validate(ctx context.Context) error {
	out, err := runCommand(ctx, "sc", "qc", d.Service)
	if err != nil {
		return fmt.Errorf("windows_service: service %q not found: %s", d.Service, firstNonEmptyLine(out, err))
	}
	return nil
}
