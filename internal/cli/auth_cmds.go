package cli

import (
	"fmt"
	"io"
	"os"
)

// loginOutput is the --json contract for "login" and the logged-in branch of
// "whoami".
type loginOutput struct {
	Server    string `json:"server"`
	Username  string `json:"username,omitempty"`
	Role      string `json:"role,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// runLogin authenticates once and persists the resulting session token to
// the local session file (session.go), so every other CLI command can omit
// --token/--username/--password afterward (authenticatedClient's fallback in
// cli.go). Never prints the token itself, in table or JSON mode.
func runLogin(flags *globalFlags, stdout, stderr io.Writer) int {
	switch {
	case flags.token != "":
		// An existing token, handed in directly - saved as-is. There is no
		// session-introspection endpoint to recover username/role from a
		// bare token (same gap runWhoami documents below), so those fields
		// stay empty until the next login or a successful whoami/command.
		if err := saveSession(savedSession{Server: flags.server, Token: flags.token}); err != nil {
			return reportError(serverErrorf("could not save session: %v", err), stderr)
		}
		out := loginOutput{Server: flags.server}
		if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, out); handled {
			return code
		}
		fmt.Fprintf(stdout, "session token saved for %s (identity unknown until the next login or command)\n", flags.server)
		return ExitOK

	case flags.username != "" && flags.password != "":
		return doLogin(flags, flags.username, flags.password, stdout, stderr)

	case isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd()):
		fmt.Fprint(stdout, "Username: ")
		username, err := readLine(os.Stdin)
		if err != nil {
			return reportError(userErrorf("reading username: %v", err), stderr)
		}
		fmt.Fprint(stdout, "Password: ")
		password, err := readPassword(os.Stdin)
		fmt.Fprintln(stdout)
		if err != nil {
			return reportError(userErrorf("reading password: %v", err), stderr)
		}
		return doLogin(flags, username, password, stdout, stderr)

	default:
		return reportError(userErrorf("authentication required: pass --username and --password (or --token), or run interactively in a terminal"), stderr)
	}
}

func doLogin(flags *globalFlags, username, password string, stdout, stderr io.Writer) int {
	client := NewClient(flags.server, "")
	if err := client.Login(username, password); err != nil {
		return reportError(err, stderr)
	}
	if err := saveSession(savedSession{
		Server:   flags.server,
		Token:    client.Token,
		Username: client.Username,
		Role:     client.Role,
	}); err != nil {
		return reportError(serverErrorf("could not save session: %v", err), stderr)
	}

	out := loginOutput{Server: flags.server, Username: client.Username, Role: client.Role, ExpiresAt: client.ExpiresAt}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, out); handled {
		return code
	}
	fmt.Fprintf(stdout, "logged in as %s (role: %s), session saved\n", client.Username, client.Role)
	return ExitOK
}

// runLogout deletes the saved session file. Idempotent - "already logged
// out" is success, not an error.
func runLogout(flags *globalFlags, stdout, stderr io.Writer) int {
	if err := deleteSession(); err != nil {
		return reportError(serverErrorf("could not remove saved session: %v", err), stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]bool{"ok": true}); handled {
		return code
	}
	fmt.Fprintln(stdout, "logged out")
	return ExitOK
}

// whoamiOutput is the --json contract for "whoami".
type whoamiOutput struct {
	Username string `json:"username,omitempty"`
	Role     string `json:"role,omitempty"`
	Server   string `json:"server,omitempty"`
	Cached   bool   `json:"cached"`
	Status   string `json:"status"`
}

// runWhoami reports the CLI's saved identity, live-verified against the
// server rather than trusted from the local file alone - a saved file only
// proves "logged in at some point," not "still authenticated now."
//
// The verification call is GET /admin/v1/nodes (via Client.Nodes) - the same
// request the "nodes" command already makes. This is a known compromise, not
// the ideal choice: the Admin API has no lightweight session-introspection
// endpoint (no GET /admin/v1/me) today, so whoami pays for a full node-list
// fetch just to confirm liveness, which is wasteful on a large fleet. Adding
// a dedicated endpoint is out of scope for this item (it would turn into an
// admin-API change, which the CLI-persistent-auth queue item's own
// blast-radius note rules out). If a lightweight /admin/v1/me endpoint is
// ever added, switch this call to that instead.
func runWhoami(flags *globalFlags, stdout, stderr io.Writer) int {
	session, err := loadSession()
	if err != nil {
		return reportError(serverErrorf("could not read saved session: %v", err), stderr)
	}
	if session == nil {
		return reportError(userErrorf("not logged in - run ollama-mesh login"), stderr)
	}

	client := NewClient(session.Server, session.Token)
	_, verifyErr := client.Nodes()

	out := whoamiOutput{Username: session.Username, Role: session.Role, Server: session.Server}
	cliErr, isCLIErr := verifyErr.(*CLIError)
	switch {
	case verifyErr == nil:
		out.Status = "active"
	case isCLIErr && cliErr.Code == ExitAuthError:
		out.Cached = true
		out.Status = "session expired or invalid - run ollama-mesh login"
		if handled, _ := emitJSON(stdout, stderr, flags.jsonOutput, out); handled {
			return ExitAuthError
		}
		printWhoami(stdout, out)
		return ExitAuthError
	default:
		out.Cached = true
		out.Status = "could not verify (server unreachable) - showing cached identity"
	}

	if handled, _ := emitJSON(stdout, stderr, flags.jsonOutput, out); handled {
		return ExitOK
	}
	printWhoami(stdout, out)
	return ExitOK
}

func printWhoami(stdout io.Writer, out whoamiOutput) {
	fmt.Fprintf(stdout, "username: %s\n", out.Username)
	fmt.Fprintf(stdout, "role:     %s\n", out.Role)
	fmt.Fprintf(stdout, "server:   %s\n", out.Server)
	if out.Cached {
		fmt.Fprintf(stdout, "status:   %s (cached)\n", out.Status)
		return
	}
	fmt.Fprintf(stdout, "status:   %s\n", out.Status)
}
