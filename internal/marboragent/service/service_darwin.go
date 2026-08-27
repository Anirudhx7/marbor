//go:build darwin

// launchd Manager implementation for the Marbor Agent's service-manager
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

// agentSupportDir/agentCertPath/agentKeyPath are the Marbor Agent's TLS
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

// CertKeyPaths returns this platform's Marbor Agent TLS certificate/key file
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
	// owner (root here) via `ps eww`, not to other local users. The agent
	// binary reads MARBOR_AGENT_SECRET from its environment at startup (no
	// legacy TOKEN fallback).
	b.WriteString("	<key>EnvironmentVariables</key>\n	<dict>\n")
	b.WriteString("		<key>MARBOR_AGENT_SECRET</key>\n		<string>" + xmlEscape(cfg.Token) + "</string>\n")
	b.WriteString("	</dict>\n")

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
// <string> element and strips XML-illegal control characters. cfg.BinaryPath/
// args/Token values are operator-controlled (paths, ports, tokens), not
// attacker input, but escaping costs nothing and keeps the plist well-formed
// if a token ever contains e.g. "&" - and a raw control character (anything
// below U+0020 except tab/LF/CR, which XML 1.0 forbids outright) would
// otherwise produce a malformed plist that `launchctl load` rejects.
func xmlEscape(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

func (launchdManager) Install(cfg Config) error {
	if err := validateCertKeyConfig(cfg); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("service: installing requires root - re-run with sudo")
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		return fmt.Errorf("service: launchctl not found on PATH: %w", err)
	}

	// P24: idempotent - a re-install/upgrade never regenerates an existing
	// cert (which would invalidate a fingerprint marbor already pinned).
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

	// P284: purge also best-effort removes the agent's TLS cert/key and log
	// file - otherwise an orphaned private key survives decommissioning,
	// enabling agent impersonation on a repurposed box. Best-effort: these
	// are cleanup, not the primary uninstall action, so a failure here
	// doesn't fail the whole command.
	if purge {
		_ = os.Remove(agentCertPath)
		_ = os.Remove(agentKeyPath)
		_ = os.Remove(launchdLogPath)
	}
	return nil
}

func (launchdManager) Start() error {
	// load/unload -w (P158), not the caller-domain start/stop verbs: the
	// generated plist sets KeepAlive=true, which launchd honors by
	// immediately respawning a job stopped via "launchctl stop" - only
	// unloading the job (as Install/Uninstall already do) actually removes
	// it from launchd's active set.
	if err := exec.Command("launchctl", "load", "-w", launchdPlistPath).Run(); err != nil {
		return fmt.Errorf("service: launchctl load: %w", err)
	}
	return nil
}

func (launchdManager) Stop() error {
	if err := exec.Command("launchctl", "unload", "-w", launchdPlistPath).Run(); err != nil {
		return fmt.Errorf("service: launchctl unload: %w", err)
	}
	return nil
}

func (launchdManager) Status() (string, error) {
	cmd := exec.Command("launchctl", "list", launchdLabel)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// "launchctl list" for a LaunchDaemon owned by another user (root)
		// fails with a permission error for a non-root caller even when the
		// daemon is actually running - conflating that with "not installed"
		// hides a real, running agent from a non-root operator running
		// `status`. os.Stat(plist) can't distinguish either (root-owned
		// files under /Library/LaunchDaemons are still world-readable), so
		// this checks stderr content first.
		if strings.Contains(stderr.String(), "Operation not permitted") || strings.Contains(stderr.String(), "Permission denied") {
			return "unknown (permission denied - re-run as root/sudo for an accurate status)", nil
		}
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
