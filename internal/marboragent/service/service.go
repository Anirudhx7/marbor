// Package service registers the Marbor Agent as a persistent,
// auto-restarting OS service (systemd on Linux, launchd on macOS, native
// Windows Service via sc.exe on Windows) - so an operator never manually
// writes a unit file, plist, or runs sc.exe themselves. See
// .local/specs/node-agent.md section 12 for the full design and why this
// shells out to each OS's own always-present service-manager tool instead of
// adding a Go dependency (golang.org/x/sys/windows/svc was considered and
// rejected for the same "zero new dependencies when the OS already provides
// this for free" reason gopsutil was rejected for host stats).
package service

import (
	"fmt"
	"time"
)

// Name is the service's registered name/label across all platforms
// (systemd unit name, launchd Label, Windows service name) - one constant so
// every platform implementation and any uninstall/status tooling agrees.
const Name = "marbor-agent"

// Config carries everything a Manager needs to (re)install the service.
// Install must be idempotent: calling it again (e.g. to change the port, or
// as part of an upgrade re-run of the install script) stops any existing
// instance, rewrites the service definition with the current BinaryPath and
// flags, and restarts - the same flow serves both "first install" and
// "upgrade in place."
type Config struct {
	// BinaryPath is the absolute path to the marbor-agent binary to run as
	// the service's command. Callers should resolve this via os.Executable()
	// so the service always points at the binary that's actually installed,
	// not a relative/PATH-dependent name that could resolve differently
	// once running under a service manager's own environment.
	BinaryPath string
	Port       int
	Token      string
	// RefreshInterval is optional; the zero value omits --refresh-interval
	// entirely and the agent falls back to its own built-in default.
	RefreshInterval time.Duration
	// CertPath/KeyPath are the agent's TLS certificate/key file paths (P24).
	// Both empty means "run plaintext" (default, matches every pre-P24
	// install unchanged). Set by each platform's Install right before
	// calling args() below, after EnsureAgentCert has confirmed the files
	// exist - never populated any other way, so a service definition never
	// points at a cert/key pair that doesn't actually exist on disk. Not
	// secret (unlike Token): the certificate is public and the private key
	// file itself carries the real protection (0600 POSIX perms / Windows
	// ACL), so passing these as plain command-line flags is safe, unlike
	// Token which must never appear in a world-readable command line.
	CertPath string
	KeyPath  string
}

// args returns the flag argument list (excluding the binary path itself; the
// bearer token is never a command-line argument - it travels via each
// platform's environment mechanism) that each platform implementation embeds
// into its service definition (systemd ExecStart, launchd ProgramArguments,
// sc.exe binPath). Centralized here so all three platforms build the exact
// same command line from the same Config fields.
//
// No leading "agent" subcommand token: BinaryPath is always the dedicated
// marbor-agent binary, which is itself the
// agent - there is no dispatcher inside it to route a subcommand through.
//
// Token is intentionally NOT included here: a service definition's command
// line is world-readable on every platform (systemd unit files/launchd
// plists are world-readable by convention, and a running process's argv is
// visible to any local user via ps/Task Manager regardless of file
// permissions) - embedding the bearer token there would let any local user
// on the node read or capture it. Each platform implementation instead
// delivers Token via that platform's environment mechanism (systemd
// EnvironmentFile, launchd EnvironmentVariables, Windows service registry
// Environment value), and the agent reads MARBOR_AGENT_SECRET from its
// process environment (it has no legacy TOKEN fallback; see
// marboragent.Run / runServiceInstall), so no agent-side change was needed to
// support this.
func (c Config) args() []string {
	a := []string{fmt.Sprintf("--port=%d", c.Port)}
	if c.RefreshInterval > 0 {
		a = append(a, fmt.Sprintf("--refresh-interval=%s", c.RefreshInterval))
	}
	if c.CertPath != "" && c.KeyPath != "" {
		a = append(a, fmt.Sprintf("--cert=%s", c.CertPath), fmt.Sprintf("--key=%s", c.KeyPath))
	}
	return a
}

// Manager installs, removes, and controls the Marbor Agent as a persistent OS
// service. Exactly one implementation is selected per GOOS by New() - never
// runtime-selected among multiple candidates the way GPUCollector is,
// because there is exactly one service manager this package supports per
// platform, not several competing ones on the same OS.
type Manager interface {
	// Install (re)registers the service with cfg and starts it, configuring
	// auto-restart on boot and on failure. Must be safe to call again later
	// with a different Config (upgrade / reconfigure) without requiring a
	// manual uninstall first. Requires root/Administrator privileges;
	// returns a clear error (never a partially-written service definition)
	// if the caller isn't elevated.
	Install(cfg Config) error
	// Uninstall stops and removes the service registration. If purge is
	// true, also deletes the installed binary at the path the service was
	// last configured to run - the default (false) only removes the service
	// registration, never deletes files the operator didn't explicitly ask
	// to remove.
	Uninstall(purge bool) error
	Start() error
	Stop() error
	// Status returns a short human-readable status string (e.g. "active
	// (running)", "inactive (dead)", "not installed").
	Status() (string, error)
}

// New returns the Manager for the current GOOS, or an error naming the
// unsupported platform - never a partial/best-effort Manager. Per
// .local/specs/node-agent.md section 12: promise the architecture (any OS
// can get a Manager implementation), not universal day-one coverage.
//
// New() itself is defined once per platform (service_linux.go,
// service_darwin.go, service_windows.go, service_unsupported.go), each
// gated by its own //go:build constraint - NOT as a single runtime
// switch on runtime.GOOS here. A runtime switch would still need every
// branch's constructor (newSystemdManager, newLaunchdManager,
// newWindowsManager) to resolve at compile time regardless of which
// branch actually runs, and each constructor lives in a file the Go
// toolchain excludes entirely for every GOOS but its own - so a single
// shared switch can never compile for any one target. Splitting New()
// itself across the per-platform files, instead, means only the one
// matching definition is ever compiled in.
