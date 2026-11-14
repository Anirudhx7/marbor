package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// marborSystemdUnitPath mirrors uninstall.sh's UNIT_PATH exactly - the marbor's
// own systemd unit is only ever written there (install.sh's
// setup_systemd_service, SERVICE=1). There is no equivalent on darwin/windows:
// install.sh falls back to a plain nohup+pidfile process on macOS, and
// install.ps1 never offers a service mode for the marbor role at all (only for
// ROLE=agent) - so this path, and everything gated on runtime.GOOS == "linux"
// below, has no counterpart to check on those platforms.
const marborSystemdUnitPath = "/etc/systemd/system/marbor.service"

// marborPidfile mirrors uninstall.sh's PIDFILE: the nohup fallback path writes
// its PID here, resolved relative to the working directory the installer (or
// operator) was run from - this subcommand must be run from that same
// directory to find it, exactly as uninstall.sh documents.
const marborPidfile = "marbor.pid"

// runUninstall implements "marbor uninstall": the Go-native counterpart
// to uninstall.sh. Only ever touches this host's marbor service/process - a
// marbor agent service (if any) is removed via "marbor-agent service
// uninstall" on its own host instead (post control-plane/Marbor-Agent binary
// split, marbor has no code path into the agent's runtime/service-manager
// logic - cmd/marbor-agent is the only entry point for it, so there is no
// Manager here to drive even if it wanted to; internal/admin does still use
// marboragent's shared frozen wire types and scope constants, which is a
// frozen protocol boundary, not an agent-runtime capability). Never touches
// marbor.db - matches
// uninstall.sh's default of always asking before deleting real state, and
// this subcommand has no interactive prompt to ask with, so the safe default
// is "never" rather than guessing.
func runUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the installed binary (default: only removes service registrations)")
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "marbor uninstall - remove the marbor's own service registration (if any) from this host\n\n")
		fmt.Fprintf(w, "Usage:\n  marbor uninstall [--purge]\n\n")
		fmt.Fprintf(w, "To remove a marbor agent, run \"marbor-agent service uninstall\" on the agent's own host instead.\n")
		fmt.Fprintf(w, "Never deletes marbor.db - remove it yourself if you also want the database gone.\n")
		fmt.Fprintf(w, "Run from the same directory the marbor server was started from, so a nohup-mode\n%s is found.\n\nFlags:\n", marborPidfile)
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	fs.Usage = func() { usage(os.Stderr) }
	for _, a := range args {
		if a == "-h" || a == "--help" {
			usage(os.Stdout)
			return
		}
	}
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "marbor: %v\n", err)
		os.Exit(1)
	}

	didSomething := false

	if runtime.GOOS == "linux" {
		if _, err := os.Stat(marborSystemdUnitPath); err == nil {
			fmt.Println("Removing marbor systemd service...")
			_ = exec.Command("systemctl", "stop", "marbor").Run()
			_ = exec.Command("systemctl", "disable", "marbor").Run()
			// Set as soon as the unit is known to exist and stop/disable ran,
			// not only on a successful remove below - a remove failure (e.g.
			// permissions) still means service state changed, so "Nothing to
			// uninstall" would be a false report otherwise.
			didSomething = true
			if err := os.Remove(marborSystemdUnitPath); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "  [!] could not remove %s: %v (needs root/sudo?)\n", marborSystemdUnitPath, err)
			} else {
				_ = exec.Command("systemctl", "daemon-reload").Run()
				fmt.Printf("  Removed %s\n", marborSystemdUnitPath)
			}
		}
	}

	serviceRemoved := didSomething
	outcome := stopPidfileProcess(marborPidfile, runtime.GOOS, runCommandOutput, os.Stdout, os.Stderr)

	if *purge {
		if binPath, err := os.Executable(); err == nil {
			if err := os.Remove(binPath); err != nil {
				fmt.Fprintf(os.Stderr, "  [!] could not remove binary %s: %v\n", binPath, err)
			} else {
				fmt.Printf("Removed binary: %s\n", binPath)
			}
		}
	}

	fmt.Println()
	fmt.Println(uninstallSummary(serviceRemoved, outcome))
	fmt.Println("marbor.db was left untouched - remove it yourself if you also want the database gone.")
}

// readRunningPidfile reads a pidfile written by install.sh's nohup fallback
// and reports whether it names a process that's still alive. A stale pidfile
// (process already gone) is treated the same as no pidfile at all - matching
// uninstall.sh, which only "stops" a pidfile-named process when kill -0
// confirms it's actually running.
func readRunningPidfile(path string) (pid int, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !pidAlive(pid) {
		return 0, false
	}
	return pid, true
}

// waitForProcessExit polls pid's liveness (via pidAlive, the same probe
// readRunningPidfile uses) until it's gone or deadline elapses.
// Reports whether the process was confirmed dead within the deadline.
func waitForProcessExit(pid int, deadline time.Duration) bool {
	const pollInterval = 100 * time.Millisecond
	for elapsed := time.Duration(0); elapsed < deadline; elapsed += pollInterval {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// stopPidfileProcess stops the background marbor process named by the
// pidfile at path, but only when its identity is positively confirmed.
// readRunningPidfile only proves the PID is alive, not that it is still the
// process the pidfile named: on a long-lived host PID recycling can point a
// stale pidfile at an unrelated process, so anything unconfirmed (lookup
// error, renamed binary, unknown OS) is left alone with the pidfile kept.
// The returned outcome says what happened.
func stopPidfileProcess(path, goos string, run commandRunner, stdout, stderr io.Writer) pidfileOutcome {
	pid, ok := readRunningPidfile(path)
	if !ok {
		return pidNone
	}
	if confirmed, reason := pidIdentityConfirmed(goos, pid, run); !confirmed {
		fmt.Fprintf(stderr, "  [!] %s names PID %d, but it could not be verified as marbor (%s; the process may belong to something else after PID reuse, the lookup tool may have failed, or the binary may have been renamed) - nothing was signalled and %s was kept.\n", path, pid, reason, path)
		return pidUnverified
	}
	fmt.Fprintf(stdout, "Stopping background marbor process (PID %d)...\n", pid)
	if !terminateFn(pid, goos, stderr) {
		return pidStopFailed
	}
	outcome := pidStopped
	if !waitExitFn(pid, stopWaitTimeout) {
		fmt.Fprintf(stderr, "  [!] PID %d did not exit within %s of being stopped - it may still be running\n", pid, stopWaitTimeout)
		outcome = pidStoppedIncomplete
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "  [!] could not remove %s: %v\n", path, err)
		outcome = pidStoppedIncomplete
	}
	return outcome
}

// Hooks so tests can drive the stop paths without a real process.
var (
	terminateFn     = terminatePid
	waitExitFn      = waitForProcessExit
	stopWaitTimeout = 5 * time.Second
)

// pidfileOutcome is what stopPidfileProcess did about the pidfile's process.
type pidfileOutcome int

const (
	pidNone              pidfileOutcome = iota // no live process named by the pidfile
	pidStopped                                 // verified as marbor and signalled
	pidUnverified                              // live PID, could not be verified; left alone
	pidStopFailed                              // verified as marbor but could not be signalled
	pidStoppedIncomplete                       // signalled, but still alive after the wait or pidfile not removed
)

// uninstallSummary builds the closing message. A marbor process that may
// still be running is always called out, whether or not a service was removed.
func uninstallSummary(serviceRemoved bool, outcome pidfileOutcome) string {
	var warn string
	switch outcome {
	case pidUnverified:
		warn = "A background process named in " + marborPidfile + " could not be verified as marbor and was left running; a marbor process may still be running."
	case pidStopFailed:
		warn = "A running marbor process named in " + marborPidfile + " could not be stopped; it may still be running."
	case pidStoppedIncomplete:
		warn = "marbor was told to stop but is not confirmed stopped, or " + marborPidfile + " could not be removed; check that no marbor process is still running and remove the pidfile if it is left."
	}
	switch {
	case outcome == pidStoppedIncomplete && !serviceRemoved:
		return "Uninstall is incomplete. " + warn
	case serviceRemoved || outcome == pidStopped:
		msg := "marbor has been uninstalled from this host."
		if warn != "" {
			msg += "\n" + warn
		}
		return msg
	case warn != "":
		return "Nothing was uninstalled. " + warn
	}
	return "Nothing to uninstall on this host: no marbor service and no background marbor process were found."
}

// terminatePid sends SIGTERM to pid, falling back to a hard Kill on Windows
// where SIGTERM is unsupported. Only called once identity is confirmed.
func terminatePid(pid int, goos string, stderr io.Writer) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(stderr, "  [!] could not find process PID %d: %v\n", pid, err)
		return false
	}
	sigErr := proc.Signal(syscall.SIGTERM)
	if sigErr == nil {
		return true
	}
	if goos != "windows" {
		fmt.Fprintf(stderr, "  [!] could not signal PID %d: %v\n", pid, sigErr)
		return false
	}
	if killErr := proc.Kill(); killErr != nil {
		fmt.Fprintf(stderr, "  [!] could not stop PID %d: %v\n", pid, killErr)
		return false
	}
	return true
}

// commandRunner runs an external command and returns its stdout. Injectable
// so identity checks are testable on every OS.
type commandRunner func(name string, args ...string) ([]byte, error)

// runCommandOutput is the production commandRunner, bounded so a hung tool
// cannot stall uninstall.
func runCommandOutput(name string, args ...string) ([]byte, error) {
	return runCommandWithTimeout(commandTimeout, name, args...)
}

// commandTimeout bounds each identity-lookup command.
const commandTimeout = 5 * time.Second

func runCommandWithTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// pidIdentityConfirmed reports whether pid is positively identified as a
// marbor process on goos. It fails closed: any lookup error, unparseable
// output, or unsupported OS yields false. When false, reason says why.
func pidIdentityConfirmed(goos string, pid int, run commandRunner) (confirmed bool, reason string) {
	switch goos {
	case "linux":
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return false, fmt.Sprintf("could not read /proc/%d/cmdline: %v", pid, err)
		}
		if !cmdlineNamesMarbor(data) {
			return false, "its command name is not marbor"
		}
		return true, ""
	case "darwin":
		out, err := run("ps", "-p", strconv.Itoa(pid), "-o", "comm=")
		if err != nil {
			return false, fmt.Sprintf("ps failed: %v", err)
		}
		if !psCommNamesMarbor(string(out)) {
			return false, "its command name is not marbor"
		}
		return true, ""
	case "windows":
		out, err := run("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
		if err != nil {
			return false, fmt.Sprintf("tasklist failed: %v", err)
		}
		ok, perr := tasklistNamesMarbor(string(out), pid)
		if perr != nil {
			return false, perr.Error()
		}
		if !ok {
			return false, "tasklist did not report marbor.exe for this PID"
		}
		return true, ""
	}
	return false, "process identity checks are not supported on " + goos
}

// cmdlineNamesMarbor reports whether the NUL-separated /proc cmdline bytes
// have an argv[0] whose base name is exactly marbor (so marbor-agent does not
// match).
func cmdlineNamesMarbor(cmdline []byte) bool {
	argv0 := string(cmdline)
	if i := strings.IndexByte(argv0, 0); i >= 0 {
		argv0 = argv0[:i]
	}
	return argv0 != "" && filepath.Base(argv0) == "marbor"
}

// psCommNamesMarbor parses "ps -o comm=" output: the base name must be
// exactly marbor.
func psCommNamesMarbor(out string) bool {
	comm := strings.TrimSpace(out)
	return comm != "" && filepath.Base(comm) == "marbor"
}

// tasklistNamesMarbor parses "tasklist /FO CSV /NH" output. Only the first
// row is considered; its image name must be marbor.exe (case-insensitive) and
// its PID field must equal pid. Localized "no tasks" text is not CSV and so
// never matches.
// A non-nil error means the output was not usable CSV.
func tasklistNamesMarbor(out string, pid int) (bool, error) {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rec, err := r.Read()
	if err != nil {
		return false, fmt.Errorf("could not parse tasklist output: %w", err)
	}
	if len(rec) < 2 {
		return false, fmt.Errorf("unexpected tasklist output %q", strings.TrimSpace(out))
	}
	return strings.EqualFold(strings.TrimSpace(rec[0]), "marbor.exe") && strings.TrimSpace(rec[1]) == strconv.Itoa(pid), nil
}
