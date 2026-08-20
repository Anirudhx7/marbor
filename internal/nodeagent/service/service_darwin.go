//go:build darwin

// launchd Manager implementation for the Node Agent's service-manager
// package. Build-tagged to darwin: New() (this file's sole exported entry
// point other than the Manager methods themselves) must be defined exactly
// once per GOOS - see service.go's New() doc comment for why a single
// shared runtime switch across all three platforms can never compile.
package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// New returns the launchd-backed Manager - the only Manager implementation
// on darwin.
func New() (Manager, error) { return newLaunchdManager(), nil }

// launchdLabel is the fixed launchd Label used across Install/Uninstall/
// Start/Stop/Status - all five must agree on the exact same string, so it's
// a single constant rather than derived per-call.
const launchdLabel = "com.marbor.agent"

// launchdPlistPath is the system-wide LaunchDaemon location (not a per-user
// LaunchAgent in ~/Library/LaunchAgents) - the agent must run regardless of
// whether any user is logged in.
const launchdPlistPath = "/Library/LaunchDaemons/" + launchdLabel + ".plist"

// launchdLogPath is where the daemon's stdout/stderr are redirected, since
// launchd itself doesn't capture output anywhere useful by default.
const launchdLogPath = "/var/log/marbor-agent.log"

// agentSupportDir/agentCertPath/agentKeyPath are the Node Agent's TLS
// certificate/key file locations on macOS (P24), matching the design's
// per-platform table: a dedicated Application Support directory (unlike
// Linux's flat /etc files) since macOS has no equivalent single
// world-readable-by-convention config directory for a launchd daemon to
// drop files into.
const (
	agentSupportDir = "/Library/Application Support/marbor-agent"
	agentCertPath   = agentSupportDir + "/agent.crt"
	agentKeyPath    = agentSupportDir + "/agent.key"
)

// CertKeyPaths returns this platform's Node Agent TLS certificate/key file
// paths - used by service_cmd.go's regen-cert subcommand and "agent service
// status" to locate the files without duplicating the path constants there.
func CertKeyPaths() (certPath, keyPath string) { return agentCertPath, agentKeyPath }

type launchdManager struct{}

func newLaunchdManager() Manager { return launchdManager{} }

// launchdPlistContent builds the plist XML for cfg. Pure string function,
// deliberately separate from any file I/O or launchctl exec calls so it's
// unit-testable without root or a real launchd - mirrors how gpu_nvidia.go
// separates parseNvidiaSMIXML from the actual exec.Command call.
func launchdPlistContent(cfg Config) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	b.WriteString("\t<key>Label</key>\n")
	b.WriteString("\t<string>" + xmlEscape(launchdLabel) + "</string>\n")

	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	b.WriteString("\t\t<string>" + xmlEscape(cfg.BinaryPath) + "</string>\n")
	for _, a := range cfg.args() {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")

	// Token travels via EnvironmentVariables, not ProgramArguments: argv is
	// visible to any local user via `ps`/Activity Monitor regardless of file
	// permissions, whereas a process's environment is only visible to its
	// owner (root here) via `ps eww`, not to other local users.
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	b.WriteString("\t\t<key>TOKEN</key>\n\t\t<string>" + xmlEscape(cfg.Token) + "</string>\n")
	b.WriteString("\t</dict>\n")

	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")

	b.WriteString("\t<key>StandardOutPath</key>\n")
	b.WriteString("\t<string>" + xmlEscape(launchdLogPath) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n")
	b.WriteString("\t<string>" + xmlEscape(launchdLogPath) + "</string>\n")

	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String()
}

// xmlEscape escapes the handful of characters that matter inside a plist
// <string> element. cfg.BinaryPath/args values are operator-controlled
// (paths, ports, tokens), not attacker input, but escaping costs nothing and
// keeps the plist well-formed if a token ever contains e.g. "&".
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

func (launchdManager) Install(cfg Config) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("service: installing requires root - re-run with sudo")
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		return fmt.Errorf("service: launchctl not found on PATH: %w", err)
	}

	// P24: idempotent - a re-install/upgrade never regenerates an existing
	// cert (which would invalidate a fingerprint the mesh already pinned).
	// MkdirAll first: unlike Linux's /etc, this directory doesn't already
	// exist by default on a fresh macOS install.
	if err := os.MkdirAll(agentSupportDir, 0755); err != nil {
		return fmt.Errorf("service: creating %s: %w", agentSupportDir, err)
	}
	if err := EnsureAgentCert(agentCertPath, agentKeyPath, false); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	cfg.CertPath, cfg.KeyPath = agentCertPath, agentKeyPath

	// 0600, not the more common 0644: this plist now carries the agent's
	// bearer token in EnvironmentVariables, and launchd (running as root)
	// doesn't need world-read access to load its own LaunchDaemon.
	content := launchdPlistContent(cfg)
	if err := os.WriteFile(launchdPlistPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("service: writing plist %s: %w", launchdPlistPath, err)
	}

	// Idempotent re-run: unload any currently-loaded copy first (ignore
	// error - it fails harmlessly if not currently loaded, which is the
	// expected case on a first install), then load the fresh plist.
	_ = exec.Command("launchctl", "unload", "-w", launchdPlistPath).Run()

	if err := exec.Command("launchctl", "load", "-w", launchdPlistPath).Run(); err != nil {
		return fmt.Errorf("service: launchctl load: %w", err)
	}
	return nil
}

func (launchdManager) Uninstall(purge bool) error {
	// Ignore error - unload fails harmlessly if the service isn't loaded.
	_ = exec.Command("launchctl", "unload", "-w", launchdPlistPath).Run()

	var binaryPath string
	if purge {
		if data, err := os.ReadFile(launchdPlistPath); err == nil {
			binaryPath, _ = extractBinaryPathFromPlist(data)
		}
	}

	if err := os.Remove(launchdPlistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: removing plist %s: %w", launchdPlistPath, err)
	}

	if purge && binaryPath != "" {
		if err := os.Remove(binaryPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("service: removing binary %s: %w", binaryPath, err)
		}
	}
	return nil
}

func (launchdManager) Start() error {
	if err := exec.Command("launchctl", "start", launchdLabel).Run(); err != nil {
		return fmt.Errorf("service: launchctl start: %w", err)
	}
	return nil
}

func (launchdManager) Stop() error {
	if err := exec.Command("launchctl", "stop", launchdLabel).Run(); err != nil {
		return fmt.Errorf("service: launchctl stop: %w", err)
	}
	return nil
}

func (launchdManager) Status() (string, error) {
	out, err := exec.Command("launchctl", "list", launchdLabel).Output()
	if err != nil {
		if _, statErr := os.Stat(launchdPlistPath); os.IsNotExist(statErr) {
			return "not installed", nil
		}
		return "not loaded", nil
	}
	return parseLaunchctlListStatus(string(out)), nil
}

// parseLaunchctlListStatus interprets `launchctl list <label>` plain-text
// output. The format has varied across macOS versions, so this parses
// forgivingly and falls back to a generic "loaded" string rather than
// failing hard on unexpected formatting.
func parseLaunchctlListStatus(out string) string {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// launchctl list <label> typically emits a dict with a "PID" key,
		// e.g. `\t"PID" = 1234;` - look for that first.
		if strings.Contains(line, `"PID"`) {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				pidStr := strings.TrimSpace(strings.Trim(strings.TrimSpace(parts[1]), ";"))
				if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
					return fmt.Sprintf("running (pid %d)", pid)
				}
			}
		}
	}

	// Older/alternate form: a single summary line "PID\tStatus\tLabel"
	// where a bare "-" in the PID column means "not currently running".
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) >= 1 {
		if fields[0] == "-" {
			return "loaded (not running)"
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
			return fmt.Sprintf("running (pid %d)", pid)
		}
	}

	return "loaded"
}

// plistDict/plistArray are a minimal subset of Apple's plist XML schema,
// just enough to decode the ProgramArguments array this package itself
// writes in launchdPlistContent - not a general plist parser. Our plist has
// exactly one top-level <array> element (ProgramArguments; RunAtLoad/
// KeepAlive are <true/> booleans, not arrays, and EnvironmentVariables is a
// <dict> this type doesn't need to decode).
type plistDict struct {
	Keys   []string     `xml:"key"`
	Arrays []plistArray `xml:"array"`
}

type plistArray struct {
	Strings []string `xml:"string"`
}

type plistRoot struct {
	Dict plistDict `xml:"dict"`
}

// extractBinaryPathFromPlist recovers the installed binary path (the first
// <string> element of ProgramArguments) from a plist previously written by
// launchdPlistContent, so Uninstall(purge=true) can delete it even though
// Uninstall takes no Config.
func extractBinaryPathFromPlist(data []byte) (string, bool) {
	var root plistRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return "", false
	}
	hasProgramArguments := false
	for _, k := range root.Dict.Keys {
		if k == "ProgramArguments" {
			hasProgramArguments = true
			break
		}
	}
	if !hasProgramArguments || len(root.Dict.Arrays) == 0 || len(root.Dict.Arrays[0].Strings) == 0 {
		return "", false
	}
	return root.Dict.Arrays[0].Strings[0], true
}
