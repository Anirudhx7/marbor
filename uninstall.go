package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// meshSystemdUnitPath mirrors uninstall.sh's UNIT_PATH exactly - the mesh's
// own systemd unit is only ever written there (install.sh's
// setup_systemd_service, SERVICE=1). There is no equivalent on darwin/windows:
// install.sh falls back to a plain nohup+pidfile process on macOS, and
// install.ps1 never offers a service mode for the mesh role at all (only for
// ROLE=agent) - so this path, and everything gated on runtime.GOOS == "linux"
// below, has no counterpart to check on those platforms.
const meshSystemdUnitPath = "/etc/systemd/system/marbor.service"

// meshPidfile mirrors uninstall.sh's PIDFILE: the nohup fallback path writes
// its PID here, resolved relative to the working directory the installer (or
// operator) was run from - this subcommand must be run from that same
// directory to find it, exactly as uninstall.sh documents.
const meshPidfile = "marbor.pid"

// runUninstall implements "marbor uninstall": the Go-native counterpart
// to uninstall.sh. Only ever touches this host's mesh service/process - a
// Node Agent service (if any) is removed via "marbor-agent service
// uninstall" on its own host instead (post control-plane/Node-Agent binary
// split, marbor no longer imports internal/nodeagent at all, so it has
// no Manager to drive even if it wanted to). Never touches marbor.db - matches
// uninstall.sh's default of always asking before deleting real state, and
// this subcommand has no interactive prompt to ask with, so the safe default
// is "never" rather than guessing.
func runUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the installed binary (default: only removes service registrations)")
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "marbor uninstall - remove the mesh's own service registration (if any) from this host\n\n")
		fmt.Fprintf(w, "Usage:\n  marbor uninstall [--purge]\n\n")
		fmt.Fprintf(w, "To remove a Node Agent, run \"marbor-agent service uninstall\" on the agent's own host instead.\n")
		fmt.Fprintf(w, "Never deletes marbor.db - remove it yourself if you also want the database gone.\n")
		fmt.Fprintf(w, "Run from the same directory the mesh was started from, so a nohup-mode\n%s is found.\n\nFlags:\n", meshPidfile)
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
		if _, err := os.Stat(meshSystemdUnitPath); err == nil {
			fmt.Println("Removing mesh systemd service...")
			_ = exec.Command("systemctl", "stop", "marbor").Run()
			_ = exec.Command("systemctl", "disable", "marbor").Run()
			if err := os.Remove(meshSystemdUnitPath); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "  [!] could not remove %s: %v (needs root/sudo?)\n", meshSystemdUnitPath, err)
			} else {
				_ = exec.Command("systemctl", "daemon-reload").Run()
				fmt.Printf("  Removed %s\n", meshSystemdUnitPath)
				didSomething = true
			}
		}
	}

	if pid, ok := readRunningPidfile(meshPidfile); ok {
		fmt.Printf("Stopping background mesh process (PID %d)...\n", pid)
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
		_ = os.Remove(meshPidfile)
		didSomething = true
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
		fmt.Println("Nothing to uninstall on this host: no mesh service and no background mesh process were found.")
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
