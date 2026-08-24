package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessDriver controls a bare process identified by a PID file, the
// fallback for a runtime with no service manager or container wrapping it.
// Requires only OS permission to signal the target process - no daemon,
// docker.sock, or service-manager access. Stop/Restart use a portable
// os.Process.Kill() rather than a Unix-only SIGTERM: Go's os.Process.Signal
// only reliably supports os.Kill cross-platform (Windows has no general
// signal delivery), so this driver deliberately does a hard stop rather
// than depend on build-tag-gated syscall.SIGTERM - a documented limitation,
// not an oversight (Architecture Law: single binary, no build-tag forks).
// StartCommand is operator-configured at registration time (5.6) since a
// bare PID-file convention alone gives no way to know how to launch the
// process fresh.
type ProcessDriver struct {
	PIDFile      string
	StartCommand []string

	// liveProc, when non-nil, is the exact *os.Process handle returned by a
	// preceding Start() call on THIS SAME driver instance (P104's
	// smallest-step fix for PID reuse). Preferring it over re-reading the pid
	// from disk and looking the process up fresh closes the
	// same-process-lifecycle reuse window: as long as this process hasn't
	// been Wait()'d/reaped, the kernel cannot recycle its pid to an unrelated
	// process. Full cross-invocation identity verification (surviving a
	// fresh driver instance, e.g. across a restart of marbor-agent itself)
	// needs platform-specific logic (no /proc on Windows) and is tracked
	// separately as BACKLOG.
	liveProc *os.Process
}

func (d *ProcessDriver) Name() string       { return "process" }
func (d *ProcessDriver) Requires() []string { return []string{"process-signal-permission"} }

func (d *ProcessDriver) Start(ctx context.Context) error {
	if len(d.StartCommand) == 0 {
		return errors.New("process: no start command configured for this node")
	}
	proc, err := startDetached(d.StartCommand[0], d.StartCommand[1:]...)
	if err != nil {
		return fmt.Errorf("process: start %q: %w", d.StartCommand[0], err)
	}
	if err := writePIDFile(d.PIDFile, proc.Pid); err != nil {
		return fmt.Errorf("process: launched pid %d but failed to write pid file %q: %w", proc.Pid, d.PIDFile, err)
	}
	d.liveProc = proc
	return nil
}

func (d *ProcessDriver) Stop(ctx context.Context) error {
	pid, err := readPIDFile(d.PIDFile)
	if err != nil {
		return fmt.Errorf("process: %w", err)
	}
	proc := d.liveProc
	if proc == nil || proc.Pid != pid {
		proc, err = findProcess(pid)
		if err != nil {
			return fmt.Errorf("process: pid %d not running: %w", pid, err)
		}
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("process: kill pid %d: %w", pid, err)
	}
	d.liveProc = nil
	return nil
}

func (d *ProcessDriver) Restart(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	return d.Start(ctx)
}

func (d *ProcessDriver) Status(ctx context.Context) (Status, error) {
	pid, err := readPIDFile(d.PIDFile)
	if err != nil {
		return Status{}, fmt.Errorf("process: %w", err)
	}
	alive := processAlive(pid)
	return Status{Running: alive, Detail: fmt.Sprintf("pid %d", pid)}, nil
}

// Logs is not supported by the Process driver: without a supervisor, there
// is no universal place stdout/stderr of an already-running process can be
// read from after the fact. Requires() names this gap so the UI can explain
// it rather than surfacing a mysterious failure (5.4).
func (d *ProcessDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	return nil, errors.New("process: log retrieval not supported without a supervisor")
}

// Validate confirms the PID file still names a live process.
func (d *ProcessDriver) Validate(ctx context.Context) error {
	pid, err := readPIDFile(d.PIDFile)
	if err != nil {
		return fmt.Errorf("process: %w", err)
	}
	if !processAlive(pid) {
		return fmt.Errorf("process: pid file %q names pid %d, which is not running", d.PIDFile, pid)
	}
	return nil
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read pid file %q: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("pid file %q does not contain a valid pid: %w", path, err)
	}
	if pid <= 0 {
		// pid<=0 has special meaning to the OS's kill/signal syscalls (0 signals
		// the whole process group, -1 signals every process the caller can
		// signal) - a corrupted/misconfigured pid file must never reach
		// Stop/Restart's proc.Kill()/proc.Signal() unguarded, or a single-node
		// operation could become a group- or fleet-wide kill.
		return 0, fmt.Errorf("pid file %q contains an invalid pid %d", path, pid)
	}
	return pid, nil
}

func writePIDFile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}
