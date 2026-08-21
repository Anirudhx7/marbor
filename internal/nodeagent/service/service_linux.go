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

// tokenEnvFilePath holds the agent's bearer token as
// MARBOR_AGENT_SECRET=<value>, written root-only (0600) so no local
// unprivileged user can read it - unlike the unit file itself (0644 by
// systemd convention) or the process's own argv, both of which are readable
// by any local user. The agent binary reads MARBOR_AGENT_SECRET from its
// environment at startup (no legacy TOKEN fallback).
const tokenEnvFilePath = "/etc/marbor-agent.env"

// agentCertPath/agentKeyPath are the Node Agent's TLS certificate/key file
// locations on Linux (P24), mirroring tokenEnvFilePath's precedent exactly:
// same directory, same 0600-secret-file treatment for the key.
const (
	agentCertPath = "/etc/marbor-agent.crt"
	agentKeyPath  = "/etc/marbor-agent.key"
)

// CertKeyPaths returns this platform's Node Agent TLS certificate/key file
// paths - used by service_cmd.go's regen-cert subcommand and "agent service
// status" to locate the files without duplicating the path constants there.
func CertKeyPaths() (certPath, keyPath string) { return agentCertPath, agentKeyPath }

// New returns the systemd-backed Manager - the only Manager implementation
// on linux.
func New() (Manager, error) { return newSystemdManager(), nil }

func newSystemdManager() Manager { return systemdManager{} }

type systemdManager struct{}

func unitPath() string {
	return systemdUnitDir + "/" + Name + ".service"
}

// quoteIfNeeded wraps s in double quotes when it contains whitespace -
// systemd.service(5) ExecStart= splits on unescaped whitespace, so an
// operator-chosen INSTALL_DIR containing a space (install.sh's INSTALL_DIR
// is user-overridable) would otherwise truncate/misparse the binary path.
// Only quoting when needed keeps the common case's output unchanged.
func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// systemdUnitContent builds the unit file text from cfg. Kept as a pure
// string function (no file I/O, no exec.Command) so it's directly testable
// without root or a real systemd - mirrors how gpu_nvidia.go's
// parseNvidiaSMIXML is split out from the exec.Command call in Collect.
func systemdUnitContent(cfg Config) string {
	parts := []string{quoteIfNeeded(cfg.BinaryPath)}
	for _, a := range cfg.args() {
		parts = append(parts, quoteIfNeeded(a))
	}
	execStart := strings.Join(parts, " ")

	return fmt.Sprintf(`[Unit]
Description=Marbor Node Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=%s
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`, tokenEnvFilePath, execStart)
}

// execStartBinary extracts the binary path (first token after "ExecStart=",
// unquoted if quoteIfNeeded quoted it) from an existing unit file's content,
// so Uninstall can find the binary to purge without needing a Config passed
// in - the Manager interface's Uninstall(purge bool) signature is shared
// across all three platform implementations and must not change. Same
// quoted-or-unquoted-leading-token handling as service_windows.go's
// parseBinaryPathFromQC.
func execStartBinary(unitContent string) string {
	for _, line := range strings.Split(unitContent, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			cmd := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
			if strings.HasPrefix(cmd, `"`) {
				if end := strings.Index(cmd[1:], `"`); end != -1 {
					return cmd[1 : end+1]
				}
			}
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

	// Written before the unit file, 0600, so a rotated token is in place
	// before systemd ever tries to (re)start the service against it.
	if err := os.WriteFile(tokenEnvFilePath, []byte("MARBOR_AGENT_SECRET="+cfg.Token+"\n"), 0600); err != nil {
		return fmt.Errorf("service: writing token env file: %w", err)
	}

	// P24: idempotent - a re-install/upgrade never regenerates an existing
	// cert (which would invalidate a fingerprint the mesh already pinned).
	if err := EnsureAgentCert(agentCertPath, agentKeyPath, false); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	cfg.CertPath, cfg.KeyPath = agentCertPath, agentKeyPath

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
	// Best-effort: the token has no value once the service is gone, but a
	// missing env file must never fail an otherwise-successful uninstall.
	_ = os.Remove(tokenEnvFilePath)

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
