package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// globalFlags holds the flags every subcommand accepts, per
// operational-interfaces.md 5.1/5.2: --json is the compatibility contract
// from the first command shipped, auth flags are shared across commands.
type globalFlags struct {
	server     string
	jsonOutput bool
	username   string
	password   string
}

// newFlagSet builds a FlagSet for a subcommand. Genuine flag-parse errors
// (not -h/--help - see parseFlags) print via flag's own default Usage
// ("Usage of <name>:" plus PrintDefaults) to stderr, which is correct GNU
// convention for an error path.
//
// --username/--password deliberately register with an empty flag default
// rather than os.Getenv(...) directly: flag.FlagSet.PrintDefaults prints
// every non-empty default verbatim as `(default "...")`, so seeding a secret
// env var as the flag's default would leak it in plain text on both the
// --help path and the genuine-parse-error path above (e.g. a mistyped flag
// on any command, with MARBOR_PASSWORD exported, would print the live
// password to stderr - straight into a CI log if that's where MARBOR_PASSWORD
// is coming from). Env fallback for these two fields happens later, via
// resolveCred, only in the places that actually consume them
// (authenticatedClient, runLogin) - never as a flag default.
//
// There is deliberately no --token flag: a bearer token passed as a CLI
// argument is visible in shell history, `ps`/Task Manager, and
// process-creation logging for the life of the process. "marbor login"
// (which persists a session to a 0600 local file) plus --username/--password
// are the only credential paths - see runLogin/authenticatedClient.
func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *globalFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := &globalFlags{}
	fs.StringVar(&g.server, "server", envOr("MARBOR_SERVER", "http://localhost:8080"), "Admin API base URL")
	fs.BoolVar(&g.jsonOutput, "json", false, "output machine-readable JSON instead of a human table")
	fs.StringVar(&g.username, "username", "", "admin username, used to log in (env MARBOR_USERNAME)")
	fs.StringVar(&g.password, "password", "", "admin password, used to log in (env MARBOR_PASSWORD)")
	return fs, g
}

// resolveCred returns flagVal if set, otherwise the named environment
// variable - the flag>env priority previously baked into the flag default
// itself (see newFlagSet's doc comment for why that was unsafe for secrets).
func resolveCred(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}

// normalizeServerURL strips a trailing slash, matching NewClient's own
// normalization (client.go) - without this, a session saved against
// "http://marbor:8080/" would never match a later "--server http://marbor:8080"
// (or vice versa), silently missing the saved-session fallback with no
// indication why.
func normalizeServerURL(s string) string {
	return strings.TrimSuffix(s, "/")
}

// hasHelpFlag reports whether -h/--help appears anywhere in args.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// parseFlags intercepts -h/--help BEFORE fs.Parse runs, printing usage to
// stdout and exiting 0 (GNU convention: a help request is not a failure).
// This has to happen before Parse, not after via fs.Usage/flag.ErrHelp -
// flag's own f.usage() hook fires identically for -h and for a genuine
// bad-flag error, which would otherwise send successful --help output to
// the same stderr stream as a real error. usage may be nil for a command
// with no actions of its own (just flags) - falls back to fs's own default
// Usage, still routed to stdout since this is still the help path.
func parseFlags(fs *flag.FlagSet, args []string, usage func(io.Writer), stdout io.Writer) (ok bool, exitCode int) {
	if hasHelpFlag(args) {
		if usage != nil {
			usage(stdout)
		} else {
			fs.SetOutput(stdout)
			fs.Usage()
		}
		return false, ExitOK
	}
	if err := fs.Parse(args); err != nil {
		return false, ExitUserError
	}
	return true, ExitOK
}

// renderTable writes rows as a two-column, tab-aligned list to w. Using
// text/tabwriter instead of hand-spaced strings means alignment is always
// correct regardless of any individual row's length - the entire point,
// since a hand-aligned table silently breaks the moment one row changes.
func renderTable(w io.Writer, indent string, rows [][2]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s%s\t%s\n", indent, r[0], r[1])
	}
	tw.Flush()
}

// authFlagsRows documents the flags every credentialed subcommand shares -
// defined once and reused by every per-command usage function below so the
// descriptions/env-var names can't drift between commands.
var authFlagsRows = [][2]string{
	{"--server string", `Admin API base URL (default "http://localhost:8080", env MARBOR_SERVER)`},
	{"--json", "output machine-readable JSON instead of a human table"},
	{"--username string", "admin username, used to log in (env MARBOR_USERNAME)"},
	{"--password string", "admin password, used to log in (env MARBOR_PASSWORD)"},
}

// printModelsUsage, printRuntimeUsage, printLoginUsage, and
// printNodeControlUsage are thin wrappers over the registry-backed
// writeHelp (help.go) - see the P83+ CLI hardening plan, migration step 4.
// The registry node they render is looked up by name/path via findCommand;
// the actual help text lives as data in registry_tree.go, not here.
//
// Known, reviewed diffs from the pre-refactor hand-written versions of these
// functions (see internal/cli/testdata/help/*.golden for the exact
// byte-for-byte old output) - none of these were fixed here because doing
// so would require either restructuring the registry tree or hard-coding
// per-command special cases into writeHelp, both out of scope for this step:
//   - models: the old table's synthetic "(none)" row (documenting the
//     fleet-wide list when models is invoked with no action) has no
//     equivalent in the registry, which only has real Sub commands.
//   - runtime: the old table grouped start/stop/restart into one row
//     ("start|stop|restart <node>") and showed inline flag hints for logs
//     ("[--lines=N]") and drain ("[--reason=X]"); the registry models
//     start/stop/restart as three separate Sub commands (so they render as
//     three rows), and the action table's name column is Name+args only,
//     with no per-flag hint rendering.
//   - node control: "accept"'s old row included its required flags inline
//     ("accept <node> --driver X --identifier Y [--start-command Z]"); the
//     generic action table renders Name+args only ("accept <node>").
func printModelsUsage(w io.Writer) { writeHelp(w, findCommand(root(), "models")) }

func printRuntimeUsage(w io.Writer) { writeHelp(w, findCommand(root(), "runtime")) }

func printLoginUsage(w io.Writer) { writeHelp(w, findCommand(root(), "login")) }

func printNodeControlUsage(w io.Writer) { writeHelp(w, findCommand(root(), "node", "control")) }

func splitFlagsAndArgs(args []string, boolFlagNames map[string]bool) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") || boolFlagNames[name] {
			continue
		}
		if i+1 < len(args) {
			flagArgs = append(flagArgs, args[i+1])
			i++
		}
	}
	return flagArgs, positional
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// authenticatedClient builds a Client for a command that requires a session
// (marbor nodes, marbor models). Missing credentials is a user error (1), not an
// auth error (4) - auth was never attempted against the server. Resolution
// priority: --username/--password flag/env > the saved session file written
// by "marbor login" (lowest priority, and only used when it was saved
// against this same --server - a saved session for one marbor must never be
// silently replayed against a different one). There is deliberately no
// --token/MARBOR_TOKEN path - see newFlagSet's doc comment.
func authenticatedClient(flags *globalFlags) (*Client, error) {
	username := resolveCred(flags.username, "MARBOR_USERNAME")
	password := resolveCred(flags.password, "MARBOR_PASSWORD")

	if username != "" && password != "" {
		client := NewClient(flags.server, "")
		if err := client.Login(username, password); err != nil {
			return nil, err
		}
		return client, nil
	}
	session, err := loadSession()
	if err != nil {
		// A local-data problem (unreadable/corrupt session file), not a
		// server/transport failure - the message already says "run marbor
		// login again," a user action, so this is a user error, not a
		// server error.
		return nil, userErrorf("could not read saved session: %v - run marbor login again", err)
	}
	if session != nil && normalizeServerURL(session.Server) == normalizeServerURL(flags.server) {
		client := NewClient(flags.server, session.Token)
		client.usingSavedSession = true
		return client, nil
	}
	return nil, userErrorf("authentication required: run marbor login, or pass --username/--password (or MARBOR_USERNAME+MARBOR_PASSWORD)")
}

// reportError prints err's message to stderr and returns the exit code it
// carries (or ExitServerError for an error that didn't originate from this
// package's classification).
func reportError(err error, stderr io.Writer) int {
	fmt.Fprintf(stderr, "error: %v\n", err)
	if cliErr, ok := err.(*CLIError); ok {
		return cliErr.Code
	}
	return ExitServerError
}

// Run parses args and dispatches to the requested subcommand, returning the
// process exit code (0/1/2/4 per operational-interfaces.md 5.2). It delegates
// entirely to the registry-driven dispatcher (dispatch.go) - see the P83+ CLI
// hardening plan, migration steps 6-8. The old hand-rolled switch statement
// that used to live here has been deleted now that every leaf command's Run
// is wired to its real implementation. Behavioral equivalence with that old
// switch was verified during the migration via a differential test
// (dispatch_differential_test.go) that compared dispatch()'s output against
// the legacy switch across ~190 argument vectors; that test was deleted once
// this function started delegating directly to dispatch() (comparing the
// dispatcher against itself would have been tautological). Current coverage
// is dispatch_p83_test.go's registry-wide trailing-argument test plus the
// full internal/cli test suite.
func Run(args []string, stdout, stderr io.Writer) int {
	return dispatchAndRun(root(), args, stdout, stderr)
}
