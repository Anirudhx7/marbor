package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
}

func (d *ProcessDriver) Name() string       { return "process" }
func (d *ProcessDriver) Requires() []string { return []string{"process-signal-permission"} }

func (d *ProcessDriver) Start(ctx context.Context) error {
	if len(d.StartCommand) == 0 {
		return errors.New("process: no start command configured for this node")
	}
	// Idempotent (P152): a retry during a slow cold start (client timeout,
	// caller retry logic) would otherwise unconditionally overwrite the PID
	// file and orphan the process it just launched, double-spawning the
	// runtime. If the existing PID-file entry already names a live process,
	// treat this as already-started rather than launching a second instance.
	if pid, err := readPIDFile(d.PIDFile); err == nil && processAlive(pid) {
		return nil
	}
	proc, err := startDetached(d.StartCommand[0], d.StartCommand[1:]...)
	if err != nil {
		return fmt.Errorf("process: start %q: %w", d.StartCommand[0], err)
	}
	if err := writePIDFile(d.PIDFile, proc.Pid); err != nil {
		// The process is now running but untracked - no PID file means
		// Status/Stop/Restart can never find it again. Best-effort kill it
		// rather than leaving an orphan the operator has no handle on.
		killErr := proc.Kill()
		if killErr != nil {
			return fmt.Errorf("process: launched pid %d but failed to write pid file %q, and failed to kill the now-untracked process: %w (write error: %v)", proc.Pid, d.PIDFile, killErr, err)
		}
		return fmt.Errorf("process: launched pid %d but failed to write pid file %q (process killed to avoid leaving it untracked): %w", proc.Pid, d.PIDFile, err)
	}
	return nil
}

func (d *ProcessDriver) Stop(ctx context.Context) error {
	pid, err := readPIDFile(d.PIDFile)
	if err != nil {
		return fmt.Errorf("process: %w", err)
	}
	proc, err := findProcess(pid)
	if err != nil {
		return fmt.Errorf("process: pid %d not running: %w", pid, err)
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("process: kill pid %d: %w", pid, err)
	}
	// Wait for the kernel to actually reap the killed process before
	// returning (code review, post-P152/P157): Kill() only submits the
	// signal - on Unix the pid stays "alive" to processAlive's signal-0
	// probe as a zombie until startDetached's background Wait() goroutine
	// reaps it. Restart() calls Stop() then Start(), and Start()'s new
	// idempotency check (P152) reads that same PID file - without this
	// wait, Restart could race Start's processAlive check against the
	// async reap, see the old (now-dead) pid as still alive, and silently
	// skip spawning a replacement process. SIGKILL is uncatchable, so this
	// loop is bounded by reap scheduling latency, not process shutdown
	// time - a short poll is sufficient in the common case.
	//
	// Must return an ERROR (not nil) whenever it gives up without
	// confirming the process is actually gone (code review, round 2): an
	// unconditional nil here reopens the exact race this wait exists to
	// close - Restart's Stop-then-Start would report the runtime "stopped"
	// and then silently skip Start's spawn because the not-yet-reaped pid
	// still reads as alive. Surfacing an error instead lets Restart's
	// `if err != nil { return err }` correctly abort rather than proceed.
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("process: pid %d still alive %s after kill (not yet reaped by the kernel) - refusing to report stopped", pid, 5*time.Second)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("process: %w waiting for pid %d to be reaped after kill", ctx.Err(), pid)
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

func (d *ProcessDriver) Restart(ctx context.Context) error {
	if err := d.Stop(ctx); err != nil {
		return err
	}
	if err := d.Start(ctx); err != nil {
		// Stop already confirmed (via its reap-wait) that the previous
		// instance is dead - clear the PID file so it doesn't keep naming a
		// process that no longer exists, and make clear in the error that
		// the old instance is gone rather than just "restart failed" (which
		// would otherwise read as "the old instance may still be running").
		_ = os.Remove(d.PIDFile)
		return fmt.Errorf("process: previous instance was stopped successfully, but starting the replacement failed: %w", err)
	}
	return nil
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

// writePIDFile writes pid via a temp-file-then-rename in path's directory,
// rather than a plain truncate-then-write, so a concurrent reader can never
// observe a corrupt/partial PID mid-write.
func writePIDFile(path string, pid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(strconv.Itoa(pid)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
