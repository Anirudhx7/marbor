package cli

import (
	"bufio"
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
// --username/--password afterward (authenticatedClient's fallback in
// cli.go). Never prints the token itself, in table or JSON mode. There is
// deliberately no way to hand in an existing token directly (no --token
// flag) - see newFlagSet's doc comment in cli.go; username/password (flag,
// env, or interactive prompt) is the only input this command accepts.
func runLogin(flags *globalFlags, stdout, stderr io.Writer) int {
	username := resolveCred(flags.username, "MARBOR_USERNAME")
	password := resolveCred(flags.password, "MARBOR_PASSWORD")

	if username != "" && password != "" {
		return doLogin(flags, username, password, stdout, stderr)
	}

	if isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd()) {
		// One shared reader across both prompts - see readPassword's doc
		// comment on why a second, independently constructed bufio.Reader
		// would risk losing input that arrived in the same burst as the
		// username line. Only the fields not already resolved above (from
		// a flag or env var) get prompted for - `login --username admin`
		// with the password typed interactively (kept out of shell history
		// and `ps`) must not have its --username silently discarded.
		reader := bufio.NewReader(os.Stdin)
		if username == "" {
			fmt.Fprint(stdout, "Username: ")
			u, err := readLine(reader)
			if err != nil {
				return reportError(userErrorf("reading username: %v", err), stderr)
			}
			username = u
		}
		if password == "" {
			fmt.Fprint(stdout, "Password: ")
			p, err := readPassword(os.Stdin.Fd(), reader)
			fmt.Fprintln(stdout)
			if err != nil {
				return reportError(userErrorf("reading password: %v", err), stderr)
			}
			password = p
		}
		return doLogin(flags, username, password, stdout, stderr)
	}

	return reportError(userErrorf("authentication required: pass --username and --password, or run interactively in a terminal"), stderr)
}

func doLogin(flags *globalFlags, username, password string, stdout, stderr io.Writer) int {
	client := NewClient(flags.server, "")
	if err := client.Login(username, password); err != nil {
		return reportError(err, stderr)
	}
	if err := saveSession(savedSession{
		Server:   normalizeServerURL(flags.server),
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

// runLogout ends the session both server-side (POST /logout, via
// Client.Logout) and locally (deleteSession). It is idempotent - "already
// logged out" (no saved session) is success, not an error, and never even
// attempts the server call in that case, since there is no token to
// identify a session with.
//
// A failed server call (unreachable server, non-2xx, timeout) is reported as
// a warning on stderr - never stdout, so --json output stays parseable -
// and does NOT fail the command: the local session file is still deleted
// and the command still exits 0. A user who types "logout" must never be
// left with a live local session just because the network call failed.
func runLogout(flags *globalFlags, stdout, stderr io.Writer) int {
	session, err := loadSession()
	if err != nil {
		return reportError(userErrorf("could not read saved session: %v", err), stderr)
	}

	serverLogout := false
	if session != nil && session.Token != "" {
		client := NewClient(session.Server, session.Token)
		if err := client.Logout(); err != nil {
			fmt.Fprintf(stderr, "warning: could not end server-side session: %v\n", err)
		} else {
			serverLogout = true
		}
	}

	if err := deleteSession(); err != nil {
		return reportError(serverErrorf("could not remove saved session: %v", err), stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{
		"ok": true, "server_logout": serverLogout,
	}); handled {
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
		return reportError(userErrorf("could not read saved session: %v", err), stderr)
	}
	if session == nil {
		return reportError(userErrorf("not logged in - run marbor login"), stderr)
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
		out.Status = "session expired or invalid - run marbor login"
		if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, out); handled {
			if code == ExitOK {
				return ExitAuthError
			}
			return code
		}
		printWhoami(stdout, out)
		return ExitAuthError
	default:
		out.Cached = true
		out.Status = "could not verify (server unreachable) - showing cached identity"
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, out); handled {
		return code
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
