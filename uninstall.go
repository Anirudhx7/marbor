package main

import (
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
// marboragent's shared R9 wire types and scope constants, which is a frozen
// protocol boundary, not an agent-runtime capability). Never touches
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

	if pid, ok := readRunningPidfile(marborPidfile); ok {
		// PID-reuse hazard (P172): readRunningPidfile only confirms pid is
		// alive, not that it's still the marbor process the pidfile
		// originally named - on a long-lived host, PID recycling can point
		// a stale pidfile at an unrelated process. Verify identity via
		// /proc/<pid>/cmdline before signaling; skip the signal (with a
		// warning) if identity can't be confirmed, rather than SIGTERMing
		// whatever currently owns that PID. Only implemented on Linux -
		// other platforms have no /proc to check, matching the pre-P172
		// signal-unconditionally behavior there.
		identityConfirmed := true
		if runtime.GOOS == "linux" {
			matches, err := pidCmdlineNamesMarbor(pid)
			identityConfirmed = err == nil && matches
		}
		if identityConfirmed {
			fmt.Printf("Stopping background marbor process (PID %d)...\n", pid)
			terminated := false
			proc, err := os.FindProcess(pid)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [!] could not find process PID %d: %v\n", pid, err)
			} else if sigErr := proc.Signal(syscall.SIGTERM); sigErr == nil {
				terminated = true
			} else if runtime.GOOS == "windows" {
				// syscall.SIGTERM is unsupported on Windows and always
				// errors - fall back to a hard Kill() so a stop actually
				// happens instead of silently reporting success while the
				// process stays alive.
				if killErr := proc.Kill(); killErr == nil {
					terminated = true
				} else {
					fmt.Fprintf(os.Stderr, "  [!] could not stop PID %d: %v\n", pid, killErr)
				}
			} else {
				fmt.Fprintf(os.Stderr, "  [!] could not signal PID %d: %v\n", pid, sigErr)
			}
			if terminated {
				if !waitForProcessExit(pid, 5*time.Second) {
					fmt.Fprintf(os.Stderr, "  [!] PID %d did not exit within 5s of being stopped - it may still be running\n", pid)
				}
				_ = os.Remove(marborPidfile)
				didSomething = true
			}
		} else {
			fmt.Fprintf(os.Stderr, "  [!] %s names PID %d, but its identity could not be confirmed as marbor (stale pidfile + PID reuse?) - skipping signal to avoid killing an unrelated process.\n", marborPidfile, pid)
		}
	}

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
	if didSomething {
		fmt.Println("marbor has been uninstalled from this host.")
	} else {
		fmt.Println("Nothing to uninstall on this host: no marbor service and no background marbor process were found.")
	}
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
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

// waitForProcessExit polls pid's liveness (via a signal-0 probe, same
// technique readRunningPidfile uses) until it's gone or deadline elapses.
// Reports whether the process was confirmed dead within the deadline.
func waitForProcessExit(pid int, deadline time.Duration) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	const pollInterval = 100 * time.Millisecond
	for elapsed := time.Duration(0); elapsed < deadline; elapsed += pollInterval {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// pidCmdlineNamesMarbor reports whether pid's argv[0] (read from
// /proc/<pid>/cmdline) names the marbor binary (P172), guarding against a
// stale pidfile whose PID has since been recycled by the OS for an
// unrelated process. Linux-only - there is no /proc on other platforms.
func pidCmdlineNamesMarbor(pid int) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, err
	}
	argv0 := string(data)
	if i := strings.IndexByte(argv0, 0); i >= 0 {
		argv0 = argv0[:i]
	}
	return strings.Contains(filepath.Base(argv0), "marbor"), nil
}
