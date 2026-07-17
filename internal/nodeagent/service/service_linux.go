//go:build linux

// Linux systemd Manager implementation. Build-tagged to linux: New() (this
// file's sole exported entry point other than the Manager methods
// themselves) must be defined exactly once per GOOS, since a shared runtime
// switch in service.go would need newSystemdManager/newLaunchdManager/
// newWindowsManager to all resolve at compile time regardless of which
// branch runs - and each lives in a file the toolchain excludes for every
// other GOOS. See service.go's New() doc comment for the full reasoning.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const systemdUnitDir = "/etc/systemd/system"

// New returns the systemd-backed Manager - the only Manager implementation
// on linux.
func New() (Manager, error) { return newSystemdManager(), nil }

func newSystemdManager() Manager { return systemdManager{} }

type systemdManager struct{}

func unitPath() string {
	return systemdUnitDir + "/" + Name + ".service"
}

// systemdUnitContent builds the unit file text from cfg. Kept as a pure
// string function (no file I/O, no exec.Command) so it's directly testable
// without root or a real systemd - mirrors how gpu_nvidia.go's
// parseNvidiaSMIXML is split out from the exec.Command call in Collect.
func systemdUnitContent(cfg Config) string {
	parts := append([]string{cfg.BinaryPath}, cfg.args()...)
	execStart := strings.Join(parts, " ")

	return fmt.Sprintf(`[Unit]
Description=ollama-mesh Node Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`, execStart)
}

// execStartBinary extracts the binary path (first whitespace-delimited token
// after "ExecStart=") from an existing unit file's content, so Uninstall can
// find the binary to purge without needing a Config passed in - the Manager
// interface's Uninstall(purge bool) signature is shared across all three
// platform implementations and must not change.
func execStartBinary(unitContent string) string {
	for _, line := range strings.Split(unitContent, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			cmd := strings.TrimPrefix(line, "ExecStart=")
			fields := strings.Fields(cmd)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemdManager) Install(cfg Config) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("service: installing requires root - re-run with sudo")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("service: systemd (systemctl) not found on this host")
	}

	content := systemdUnitContent(cfg)
	if err := os.WriteFile(unitPath(), []byte(content), 0644); err != nil {
		return fmt.Errorf("service: writing unit file: %w", err)
	}

	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", Name); err != nil {
		return err
	}
	// restart (not start) so a re-run with a changed Config (rotated token,
	// upgrade) always picks up the new unit content even if the previous
	// instance was already running.
	if err := runSystemctl("restart", Name); err != nil {
		return err
	}
	return nil
}

func (systemdManager) Uninstall(purge bool) error {
	var binaryPath string
	if content, err := os.ReadFile(unitPath()); err == nil {
		binaryPath = execStartBinary(string(content))
	}

	// Ignore errors from stop/disable - uninstalling an already-stopped or
	// never-installed service should not itself be an error.
	_ = runSystemctl("stop", Name)
	_ = runSystemctl("disable", Name)

	if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: removing unit file: %w", err)
	}

	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}

	if purge && binaryPath != "" {
		if err := os.Remove(binaryPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("service: removing binary %q: %w", binaryPath, err)
		}
	}
	return nil
}

func (systemdManager) Start() error { return runSystemctl("start", Name) }
func (systemdManager) Stop() error  { return runSystemctl("stop", Name) }

func (systemdManager) Status() (string, error) {
	if _, err := os.Stat(unitPath()); os.IsNotExist(err) {
		return "not installed", nil
	}

	active := strings.TrimSpace(runSystemctlOutput("is-active", Name))
	enabled := strings.TrimSpace(runSystemctlOutput("is-enabled", Name))
	if active == "" {
		active = "unknown"
	}
	if enabled == "" {
		enabled = "unknown"
	}
	return fmt.Sprintf("%s (%s)", active, enabled), nil
}

// runSystemctlOutput runs systemctl and returns stdout regardless of exit
// code - is-active/is-enabled exit non-zero for perfectly normal states
// ("inactive", "disabled"), so a non-zero exit here is not an error, it's the
// status.
func runSystemctlOutput(args ...string) string {
	out, _ := exec.Command("systemctl", args...).Output()
	return string(out)
}
