package cli

// agent.go - `marbor agent get/enable/disable/regenerate` (P-A2-09b),
// `marbor node control clear` (P-A2-09c), and `marbor users change-password`
// / `marbor users skip-password-change` (P-A2-09d). All had full UI
// coverage but no CLI per the A2 three-surface-parity audit.

import (
	"bufio"
	"fmt"
	"io"
	"os"
)

func printAgentUsage(w io.Writer) { writeHelp(w, findCommand(root(), "node", "agent")) }

// runAgentGet implements `marbor agent get <node>`. Never prints a token -
// GetMarborAgent's response never carries one (R8).
func runAgentGet(flags *globalFlags, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	info, err := client.GetMarborAgent(node)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, info); handled {
		return code
	}
	fmt.Fprintf(stdout, "node=%s enabled=%v port=%d scope=%s scheme=%s\n", info.Node, info.Enabled, info.Port, info.Scope, info.Scheme)
	return ExitOK
}

// runAgentEnable implements `marbor agent enable <node> --port N
// [--scheme http|https]`. Prints the one-line install command containing a
// short-lived, single-use enrollment code - never the permanent token
// itself in this text form (P50); the raw "token" field is also present in
// --json output for completeness but should not be pasted into shell
// history / chat any more than the install command should.
func runAgentEnable(flags *globalFlags, node string, port int, scheme string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	result, err := client.EnableMarborAgent(node, port, scheme)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}
	fmt.Fprintf(stdout, "marbor agent enabled for %q (port %d, scheme %s)\n", node, result.Port, result.Scheme)
	fmt.Fprintf(stdout, "install (unix):    %s\n", result.InstallCommand)
	fmt.Fprintf(stdout, "install (windows): %s\n", result.InstallCommandWindows)
	return ExitOK
}

// runAgentDisable implements `marbor agent disable <node> [--yes]`.
// Destructive per R10 (drops the agent config + any pinned TLS fingerprint
// on the host): requires --yes or an interactive TTY confirmation.
func runAgentDisable(flags *globalFlags, node string, yes bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("disable marbor agent for", node, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DisableMarborAgent(node); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "node": node, "disabled": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "marbor agent disabled for %q\n", node)
	return ExitOK
}

// runAgentRegenerate implements `marbor agent regenerate <node>`.
func runAgentRegenerate(flags *globalFlags, node string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	result, err := client.RegenerateMarborAgentToken(node)
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}
	fmt.Fprintf(stdout, "marbor agent token regenerated for %q\n", node)
	fmt.Fprintf(stdout, "install (unix):    %s\n", result.InstallCommand)
	fmt.Fprintf(stdout, "install (windows): %s\n", result.InstallCommandWindows)
	return ExitOK
}

// runNodeControlClear implements `marbor node control clear <node>
// [--yes]`. Destructive per R10 (drops the operator-accepted control
// driver, disabling runtime start/stop/restart/logs until re-accepted).
func runNodeControlClear(flags *globalFlags, node string, yes bool, stdout, stderr io.Writer) int {
	if err := requireConfirm("clear the control driver for", node, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.ClearNodeControl(node); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "node": node, "cleared": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "control driver cleared for %q\n", node)
	return ExitOK
}

// runUsersChangePassword implements `marbor users change-password`. Follows
// the exact same masked-interactive-prompt discipline as runLogin
// (termpw.go / readPassword) so a password is never left in shell history
// or a --flag visible to `ps` - unlike login, there is no flag/env
// fallback at all for the new password, only interactive input.
func runUsersChangePassword(flags *globalFlags, stdout, stderr io.Writer) int {
	if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stdout.Fd()) {
		return reportError(userErrorf("change-password requires an interactive terminal (masked password prompts)"), stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(stdout, "Current password (blank if this is a forced first-login change): ")
	current, err := readPassword(os.Stdin.Fd(), reader)
	fmt.Fprintln(stdout)
	if err != nil {
		return reportError(userErrorf("reading current password: %v", err), stderr)
	}
	fmt.Fprint(stdout, "New password: ")
	newPass, err := readPassword(os.Stdin.Fd(), reader)
	fmt.Fprintln(stdout)
	if err != nil {
		return reportError(userErrorf("reading new password: %v", err), stderr)
	}
	fmt.Fprint(stdout, "Confirm new password: ")
	confirm, err := readPassword(os.Stdin.Fd(), reader)
	fmt.Fprintln(stdout)
	if err != nil {
		return reportError(userErrorf("reading password confirmation: %v", err), stderr)
	}
	if newPass != confirm {
		return reportError(userErrorf("new password and confirmation do not match"), stderr)
	}
	if newPass == "" {
		return reportError(userErrorf("new password must not be empty"), stderr)
	}

	if err := client.ChangePassword(current, newPass); err != nil {
		return reportError(err, stderr)
	}
	if err := updateSavedSessionToken(flags, client); err != nil {
		fmt.Fprintf(stderr, "warning: password changed, but could not update saved session: %v\n", err)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true}); handled {
		return code
	}
	fmt.Fprintln(stdout, "password changed")
	return ExitOK
}

// updateSavedSessionToken rewrites the saved session file with client's
// current token (rotated by ChangePassword/SkipPasswordChange server-side),
// preserving the session's existing Username/Role - authenticatedClient's
// saved-session path never populates client.Username/client.Role (only
// Login does), so overwriting with those fields directly would silently
// blank out identity info whoami later reads (code review self-check).
func updateSavedSessionToken(flags *globalFlags, client *Client) error {
	existing, err := loadSession()
	username, role := client.Username, client.Role
	if err == nil && existing != nil {
		if username == "" {
			username = existing.Username
		}
		if role == "" {
			role = existing.Role
		}
	}
	return saveSession(savedSession{
		Server:   normalizeServerURL(flags.server),
		Token:    client.Token,
		Username: username,
		Role:     role,
	})
}

// runUsersSkipPasswordChange implements `marbor users skip-password-change`.
func runUsersSkipPasswordChange(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.SkipPasswordChange(); err != nil {
		return reportError(err, stderr)
	}
	if err := updateSavedSessionToken(flags, client); err != nil {
		fmt.Fprintf(stderr, "warning: skipped, but could not update saved session: %v\n", err)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true}); handled {
		return code
	}
	fmt.Fprintln(stdout, "password change skipped for this session")
	return ExitOK
}
