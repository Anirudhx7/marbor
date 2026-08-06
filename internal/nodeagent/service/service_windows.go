//go:build windows

package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// windowsManager implements Manager on top of sc.exe, the service-control
// tool shipped with every supported Windows version. No new Go module
// dependency (golang.org/x/sys/windows/svc was considered and rejected) -
// this is the same "shell out to a native OS tool" pattern already used for
// nvidia-smi elsewhere in this codebase.
type windowsManager struct{}

// New returns the sc.exe-backed Manager - the only Manager implementation
// on windows. Defined once per GOOS (see service.go's New() doc comment for
// why a single shared runtime switch across all three platforms can never
// compile).
func New() (Manager, error) { return newWindowsManager(), nil }

func newWindowsManager() Manager { return windowsManager{} }

// windowsBinPath builds the single command-line string sc.exe's binPath=
// value expects: the binary path, always double-quoted (it may contain
// spaces, e.g. "Program Files", and quoting unconditionally is simpler and
// always correct than quoting only when needed), followed by the
// space-separated cfg.args().
func windowsBinPath(cfg Config) string {
	parts := make([]string, 0, len(cfg.args())+1)
	parts = append(parts, `"`+cfg.BinaryPath+`"`)
	parts = append(parts, cfg.args()...)
	return strings.Join(parts, " ")
}

// isElevated reports whether the current process is running with
// Administrator privileges. There is no portable os.Geteuid()-equivalent on
// Windows without a new dependency, so this shells out to "net session":
// it succeeds (exit 0) only when run elevated, and fails (typically exit
// code 2, "Access is denied") otherwise. Any error from running it is
// treated as "not elevated."
func isElevated() bool {
	err := exec.Command("net", "session").Run()
	return err == nil
}

// runSC runs sc.exe with the given arguments and returns combined
// stdout+stderr along with any error.
func runSC(args ...string) (string, error) {
	cmd := exec.Command("sc", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// serviceRegistryPath is where sc.exe registers Name, in PowerShell's
// registry-provider drive syntax; setting an Environment value here (read by
// every Windows service host at process start) is how the token reaches the
// agent without putting it in binPath, which sc qc and Task Manager's
// "Command line" column both expose to any local user.
func serviceRegistryPath() string {
	return `HKLM:\SYSTEM\CurrentControlSet\Services\` + Name
}

// setServiceTokenEnvCommand builds the powershell.exe invocation that writes
// TOKEN=<token> as the service's Environment (REG_MULTI_SZ - the type
// Windows services read their environment block from) registry value. The
// token is delivered via the command's Stdin, never as a command-line
// argument: unlike reg.exe's "/d TOKEN=<token>" form, this keeps the token
// out of Task Manager's "Command line" column, sc qc, WMI
// Win32_Process.CommandLine, and Sysmon Event ID 1 - the same class of
// exposure windowsBinPath already keeps --token out of, just via a
// different native tool that only accepts secrets as an argument. Split out
// from setServiceTokenEnv so a test can assert on the built command's Args
// without requiring an elevated Windows box to actually run it.
func setServiceTokenEnvCommand(token string) *exec.Cmd {
	script := `$t = [Console]::In.ReadLine(); Set-ItemProperty -Path '` + serviceRegistryPath() + `' -Name Environment -Value @("TOKEN=$t") -Type MultiString`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Stdin = strings.NewReader(token + "\n")
	return cmd
}

// setServiceTokenEnv writes TOKEN=<token> as the service's Environment
// registry value via powershell.exe (same "shell out to a native OS tool"
// pattern as sc.exe - no new Go module dependency), passing the token via
// stdin rather than argv (see setServiceTokenEnvCommand).
func setServiceTokenEnv(token string) error {
	cmd := setServiceTokenEnvCommand(token)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("set service Environment: %w: %s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}

// serviceExists reports whether Name is already registered, by running
// "sc query <Name>". Any error from that query (including the well-known
// "The specified service does not exist" failure) is treated as "doesn't
// exist yet."
func serviceExists() bool {
	_, err := runSC("query", Name)
	return err == nil
}

func (windowsManager) Install(cfg Config) error {
	if !isElevated() {
		return fmt.Errorf("service: installing requires Administrator - re-run this command from an elevated (Run as Administrator) terminal")
	}

	binPath := windowsBinPath(cfg)

	var out string
	var err error
	if serviceExists() {
		out, err = runSC("config", Name, "binPath=", binPath, "start=", "auto")
		if err != nil {
			return fmt.Errorf("service: sc config failed: %w: %s", err, out)
		}
	} else {
		out, err = runSC("create", Name, "binPath=", binPath, "start=", "auto", "DisplayName=", "ollama-mesh Node Agent")
		if err != nil {
			return fmt.Errorf("service: sc create failed: %w: %s", err, out)
		}
	}

	if err := setServiceTokenEnv(cfg.Token); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	// Stop it if currently running (ignore errors - it may not be running),
	// then start it fresh so the new binPath/token takes effect. "sc stop"
	// only requests the stop and returns immediately, without waiting for
	// the process to actually exit - starting right away can race the old
	// process still holding cfg.Port, surfacing as a bind failure on an
	// otherwise-routine re-install/upgrade. systemctl's "restart" on Linux
	// (service_linux.go) blocks until the unit is actually down first, so
	// this race has no equivalent there.
	_, _ = runSC("stop", Name)
	waitForStopped(10 * time.Second)
	if out, err := runSC("start", Name); err != nil {
		return fmt.Errorf("service: sc start failed: %w: %s", err, out)
	}

	return nil
}

// waitForStopped polls "sc query" until Name is no longer RUNNING or the
// timeout elapses, so a stop-then-start reinstall doesn't race the old
// process for the same port.
func waitForStopped(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := runSC("query", Name)
		if err != nil || !strings.Contains(out, "RUNNING") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (windowsManager) Uninstall(purge bool) error {
	var binPathToRemove string
	if purge {
		if out, err := runSC("qc", Name); err == nil {
			binPathToRemove = parseBinaryPathFromQC(out)
		}
	}

	// Stop before delete - ignore errors, it may not be running.
	_, _ = runSC("stop", Name)
	// Ignore "service does not exist" style failures - uninstalling an
	// already-absent service should not itself error.
	_, _ = runSC("delete", Name)

	if purge && binPathToRemove != "" {
		if err := os.Remove(binPathToRemove); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("service: purge failed to remove binary %q: %w", binPathToRemove, err)
		}
	}

	return nil
}

func (windowsManager) Start() error {
	if out, err := runSC("start", Name); err != nil {
		return fmt.Errorf("service: sc start failed: %w: %s", err, out)
	}
	return nil
}

func (windowsManager) Stop() error {
	if out, err := runSC("stop", Name); err != nil {
		return fmt.Errorf("service: sc stop failed: %w: %s", err, out)
	}
	return nil
}

func (windowsManager) Status() (string, error) {
	out, err := runSC("query", Name)
	if err != nil {
		// Query failing generally means the service isn't registered - this
		// is a query, not an assertion the service must exist.
		return "not installed", nil
	}
	return parseStateFromQuery(out), nil
}

// parseBinaryPathFromQC extracts the executable path from "sc qc <Name>"
// output's BINARY_PATH_NAME line, e.g.:
//
//	BINARY_PATH_NAME  : "C:\Program Files\ollama-mesh\ollama-mesh.exe" agent --port=9200
//
// It returns the first quoted token if present, otherwise the first
// whitespace-delimited token after the colon. Returns "" if the line can't
// be found.
func parseBinaryPathFromQC(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "BINARY_PATH_NAME") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx == -1 {
			return ""
		}
		value := strings.TrimSpace(line[idx+1:])
		if value == "" {
			return ""
		}
		if value[0] == '"' {
			if end := strings.Index(value[1:], "\""); end != -1 {
				return value[1 : end+1]
			}
			return strings.Trim(value, "\"")
		}
		if sp := strings.IndexAny(value, " \t"); sp != -1 {
			return value[:sp]
		}
		return value
	}
	return ""
}

// parseStateFromQuery extracts the human-readable state word from "sc query
// <Name>" output's STATE line, e.g.:
//
//	STATE              : 4  RUNNING
//
// and returns it lowercased (e.g. "running"). Parsing is forgiving of
// formatting differences across Windows versions - if the STATE line can't
// be found, the raw trimmed output is returned instead of hard-failing.
func parseStateFromQuery(out string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "STATE") {
			continue
		}
		idx := strings.Index(trimmed, ":")
		if idx == -1 {
			continue
		}
		fields := strings.Fields(trimmed[idx+1:])
		if len(fields) == 0 {
			continue
		}
		// Fields are typically ["4", "RUNNING"] - the state word is the last
		// non-numeric field.
		word := fields[len(fields)-1]
		if word != "" {
			return strings.ToLower(word)
		}
	}
	return strings.ToLower(strings.TrimSpace(out))
}
