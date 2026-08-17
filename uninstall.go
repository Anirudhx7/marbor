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

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
)

// meshSystemdUnitPath mirrors uninstall.sh's UNIT_PATH exactly - the mesh's
// own systemd unit is only ever written there (install.sh's
// setup_systemd_service, SERVICE=1). There is no equivalent on darwin/windows:
// install.sh falls back to a plain nohup+pidfile process on macOS, and
// install.ps1 never offers a service mode for the mesh role at all (only for
// ROLE=agent) - so this path, and everything gated on runtime.GOOS == "linux"
// below, has no counterpart to check on those platforms.
const meshSystemdUnitPath = "/etc/systemd/system/ollama-mesh.service"

// meshPidfile mirrors uninstall.sh's PIDFILE: the nohup fallback path writes
// its PID here, resolved relative to the working directory the installer (or
// operator) was run from - this subcommand must be run from that same
// directory to find it, exactly as uninstall.sh documents.
const meshPidfile = "ollama-mesh.pid"

// runUninstall implements "ollama-mesh uninstall": the Go-native counterpart
// to uninstall.sh, extended to also remove the Node Agent service if one is
// installed on this same host (uninstall.sh deliberately does not - see its
// own header comment - because it has no access to the agent service
// package's Manager; this binary does). Never touches mesh.db - matches
// uninstall.sh's default of always asking before deleting real state, and
// this subcommand has no interactive prompt to ask with, so the safe default
// is "never" rather than guessing.
func runUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete the installed binary (default: only removes service registrations)")
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "ollama-mesh uninstall - remove the mesh's own service registration (if any) and the\nNode Agent's service registration (if any) from this host\n\n")
		fmt.Fprintf(w, "Usage:\n  ollama-mesh uninstall [--purge]\n\n")
		fmt.Fprintf(w, "Never deletes mesh.db - remove it yourself if you also want the database gone.\n")
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
		fmt.Fprintf(os.Stderr, "ollama-mesh: %v\n", err)
		os.Exit(1)
	}

	didSomething := false

	if runtime.GOOS == "linux" {
		if _, err := os.Stat(meshSystemdUnitPath); err == nil {
			fmt.Println("Removing mesh systemd service...")
			_ = exec.Command("systemctl", "stop", "ollama-mesh").Run()
			_ = exec.Command("systemctl", "disable", "ollama-mesh").Run()
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

	agentUninstalled, err := nodeagent.UninstallAgentServiceIfInstalled(*purge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Node Agent service uninstall failed: %v\n", err)
	} else if agentUninstalled {
		fmt.Println("Removed the Node Agent service.")
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
		fmt.Println("ollama-mesh has been uninstalled from this host.")
	} else {
		fmt.Println("Nothing to uninstall on this host: no mesh service, no background mesh process, and no Node Agent service were found.")
	}
	fmt.Println("mesh.db was left untouched - remove it yourself if you also want the database gone.")
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
