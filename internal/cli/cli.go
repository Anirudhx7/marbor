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
// on any command, with MESH_PASSWORD exported, would print the live
// password to stderr - straight into a CI log if that's where MESH_PASSWORD
// is coming from). Env fallback for these two fields happens later, via
// resolveCred, only in the places that actually consume them
// (authenticatedClient, runLogin) - never as a flag default.
//
// There is deliberately no --token flag: a bearer token passed as a CLI
// argument is visible in shell history, `ps`/Task Manager, and
// process-creation logging for the life of the process. "ollama-mesh login"
// (which persists a session to a 0600 local file) plus --username/--password
// are the only credential paths - see runLogin/authenticatedClient.
func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *globalFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := &globalFlags{}
	fs.StringVar(&g.server, "server", envOr("MESH_SERVER", "http://localhost:8080"), "Admin API base URL")
	fs.BoolVar(&g.jsonOutput, "json", false, "output machine-readable JSON instead of a human table")
	fs.StringVar(&g.username, "username", "", "admin username, used to log in (env MESH_USERNAME)")
	fs.StringVar(&g.password, "password", "", "admin password, used to log in (env MESH_PASSWORD)")
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
// "http://mesh:8080/" would never match a later "--server http://mesh:8080"
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
	{"--server string", `Admin API base URL (default "http://localhost:8080", env MESH_SERVER)`},
	{"--json", "output machine-readable JSON instead of a human table"},
	{"--username string", "admin username, used to log in (env MESH_USERNAME)"},
	{"--password string", "admin password, used to log in (env MESH_PASSWORD)"},
}

func printModelsUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: ollama-mesh models [action] [args] [flags]\n\nActions:\n")
	renderTable(w, "  ", [][2]string{
		{"(none)", "list models known across the fleet (aggregate view)"},
		{"pull <node> <model>", "start pulling a model onto a node (async - does not wait for completion)"},
		{"delete <node> <model>", "delete a model from a node's local storage"},
		{"unload <node> <model>", "unload a model from a node's warm state"},
		{"list <node>", "list models present on a node's local storage (per-node, not the fleet-wide aggregate above)"},
	})
	fmt.Fprint(w, "\nFlags:\n")
	renderTable(w, "  ", authFlagsRows)
}

func printRuntimeUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: ollama-mesh runtime <action> <node> [flags]\n\nActions:\n")
	renderTable(w, "  ", [][2]string{
		{"start|stop|restart <node>", "start/stop/restart the node's inference runtime process"},
		{"logs <node> [--lines=N]", "fetch recent log lines from the node's runtime process"},
		{"drain <node> [--reason=X]", "mark the node draining (stop routing new requests to it)"},
		{"undrain <node>", `reverse "runtime drain"`},
		{"health <node>", "run an on-demand active liveness probe on the node"},
	})
	fmt.Fprint(w, "\nFlags:\n")
	renderTable(w, "  ", authFlagsRows)
	fmt.Fprint(w, `
"start|stop|restart" requires the target node to have an operator-accepted
control driver (see "node control accept") - a node with none configured
returns an error rather than guessing one.

"logs" is a point-in-time snapshot, not a live tail. A node whose control
driver has no real log source (e.g. a bare PID-file process with no
supervisor) returns a clear "not supported" error.
`)
}

// loginFlagsRows is authFlagsRows verbatim - login shares the exact same
// credential flags as every other credentialed command, since it is itself
// how a session is produced.
var loginFlagsRows = authFlagsRows

func printLoginUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: ollama-mesh login [flags]\n\n")
	fmt.Fprint(w, "Authenticates once and saves the resulting session to a local file (0600,\n")
	fmt.Fprint(w, "under the OS user config dir) so other commands can omit --username/\n")
	fmt.Fprint(w, "--password afterward. Run without --username/--password in a terminal to\n")
	fmt.Fprint(w, "be prompted interactively (password input is not echoed).\n\n")
	fmt.Fprint(w, "Flags:\n")
	renderTable(w, "  ", loginFlagsRows)
}

func printNodeControlUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: ollama-mesh node control <action> <node> [flags]\n\nActions:\n")
	renderTable(w, "  ", [][2]string{
		{"probe <node>", "show a node's control-driver status (configured + discovered)"},
		{"accept <node> --driver X --identifier Y [--start-command Z]", "accept a control driver + identifier for a node"},
	})
	fmt.Fprint(w, "\nFlags:\n")
	renderTable(w, "  ", authFlagsRows)
}

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
// (mesh nodes, mesh models). Missing credentials is a user error (1), not an
// auth error (4) - auth was never attempted against the server. Resolution
// priority: --username/--password flag/env > the saved session file written
// by "ollama-mesh login" (lowest priority, and only used when it was saved
// against this same --server - a saved session for one mesh must never be
// silently replayed against a different one). There is deliberately no
// --token/MESH_TOKEN path - see newFlagSet's doc comment.
func authenticatedClient(flags *globalFlags) (*Client, error) {
	username := resolveCred(flags.username, "MESH_USERNAME")
	password := resolveCred(flags.password, "MESH_PASSWORD")

	if username != "" && password != "" {
		client := NewClient(flags.server, "")
		if err := client.Login(username, password); err != nil {
			return nil, err
		}
		return client, nil
	}
	session, err := loadSession()
	if err != nil {
		return nil, serverErrorf("could not read saved session: %v - run ollama-mesh login again", err)
	}
	if session != nil && normalizeServerURL(session.Server) == normalizeServerURL(flags.server) {
		client := NewClient(flags.server, session.Token)
		client.usingSavedSession = true
		return client, nil
	}
	return nil, userErrorf("authentication required: run ollama-mesh login, or pass --username/--password (or MESH_USERNAME+MESH_PASSWORD)")
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

const usage = `ollama-mesh - CLI client for the ollama-mesh Admin API

Usage:
  ollama-mesh <command> [flags]

Commands:
  version                                    print CLI and (if reachable) server version
  status                                     print mesh health/status summary
  login                                      authenticate once and save the session locally (recommended)
  logout                                     remove the saved session
  whoami                                     show the CLI's saved identity (live-verified)
  nodes                                      list nodes known to the mesh
  models [action] ...                        fleet-wide list, or pull/delete/unload/list on one node
  runtime <action> <node> [flags]            start/stop/restart/logs/drain/undrain/health on one node
  node control probe <node>                  show a node's control-driver status (configured + discovered)
  node control accept <node> --driver X --identifier Y [--start-command Z]
                                              accept a control driver + identifier for a node
  key set-local-only <name> <true|false>    block (or re-allow) cloud fallback for one API key
  key set-allow-local-degradation <name> <true|false>
                                              let (or forbid) one API key receive a local alternate model
  spill                                       show per-key, per-provider local-vs-cloud request counts

Run "ollama-mesh <command> --help" for the full list of actions and flags for
that command.

Global flags:
  --server string      Admin API base URL (default "http://localhost:8080", env MESH_SERVER)
  --json                output machine-readable JSON instead of a human table
  --username string     admin username, used to log in (env MESH_USERNAME)
  --password string      admin password, used to log in (env MESH_PASSWORD)

"nodes", "models", "runtime", and "node control" require credentials: run
"ollama-mesh login" once (recommended), or pass --username+--password (or
MESH_USERNAME+MESH_PASSWORD) on every invocation instead.
"version" and "status" do not require credentials.
`

// Run parses args and dispatches to the requested subcommand, returning the
// process exit code (0/1/2/4 per operational-interfaces.md 5.2).
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitUserError
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return ExitOK
	case "version":
		fs, flags := newFlagSet("version", stderr)
		if ok, code := parseFlags(fs, rest, nil, stdout); !ok {
			return code
		}
		return runVersion(flags, stdout, stderr)
	case "status":
		fs, flags := newFlagSet("status", stderr)
		if ok, code := parseFlags(fs, rest, nil, stdout); !ok {
			return code
		}
		return runStatus(flags, stdout, stderr)
	case "login":
		fs, flags := newFlagSet("login", stderr)
		if ok, code := parseFlags(fs, rest, printLoginUsage, stdout); !ok {
			return code
		}
		return runLogin(flags, stdout, stderr)
	case "logout":
		fs, flags := newFlagSet("logout", stderr)
		if ok, code := parseFlags(fs, rest, nil, stdout); !ok {
			return code
		}
		return runLogout(flags, stdout, stderr)
	case "whoami":
		fs, flags := newFlagSet("whoami", stderr)
		if ok, code := parseFlags(fs, rest, nil, stdout); !ok {
			return code
		}
		return runWhoami(flags, stdout, stderr)
	case "nodes":
		fs, flags := newFlagSet("nodes", stderr)
		if ok, code := parseFlags(fs, rest, nil, stdout); !ok {
			return code
		}
		return runNodes(flags, stdout, stderr)
	case "models":
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			switch rest[0] {
			case "pull":
				fs, flags := newFlagSet("models pull", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if ok, code := parseFlags(fs, flagArgs, printModelsUsage, stdout); !ok {
					return code
				}
				if len(positional) != 2 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models pull <node> <model> [flags]")
					return ExitUserError
				}
				return runModelsPull(flags, positional[0], positional[1], stdout, stderr)
			case "delete":
				fs, flags := newFlagSet("models delete", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if ok, code := parseFlags(fs, flagArgs, printModelsUsage, stdout); !ok {
					return code
				}
				if len(positional) != 2 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models delete <node> <model> [flags]")
					return ExitUserError
				}
				return runModelsDelete(flags, positional[0], positional[1], stdout, stderr)
			case "unload":
				fs, flags := newFlagSet("models unload", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if ok, code := parseFlags(fs, flagArgs, printModelsUsage, stdout); !ok {
					return code
				}
				if len(positional) != 2 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models unload <node> <model> [flags]")
					return ExitUserError
				}
				return runModelsUnload(flags, positional[0], positional[1], stdout, stderr)
			case "list":
				fs, flags := newFlagSet("models list", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if ok, code := parseFlags(fs, flagArgs, printModelsUsage, stdout); !ok {
					return code
				}
				if len(positional) != 1 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models list <node> [flags]")
					return ExitUserError
				}
				return runModelsList(flags, positional[0], stdout, stderr)
			default:
				fmt.Fprintf(stderr, "unknown models action %q (want pull, delete, unload, or list)\n\n", rest[0])
				printModelsUsage(stderr)
				return ExitUserError
			}
		}
		fs, flags := newFlagSet("models", stderr)
		if ok, code := parseFlags(fs, rest, printModelsUsage, stdout); !ok {
			return code
		}
		return runModels(flags, stdout, stderr)
	case "runtime":
		if len(rest) < 1 {
			printRuntimeUsage(stderr)
			return ExitUserError
		}
		action := rest[0]
		if action == "-h" || action == "--help" {
			printRuntimeUsage(stdout)
			return ExitOK
		}
		switch action {
		case "start", "stop", "restart":
			fs, flags := newFlagSet("runtime "+action, stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printRuntimeUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintf(stderr, "usage: ollama-mesh runtime %s <node> [flags]\n", action)
				return ExitUserError
			}
			return runRuntimeAction(flags, action, positional[0], stdout, stderr)
		case "logs":
			fs, flags := newFlagSet("runtime logs", stderr)
			lines := fs.Int("lines", 0, "number of log lines to fetch (0 = server default)")
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printRuntimeUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime logs <node> [--lines=N] [flags]")
				return ExitUserError
			}
			return runRuntimeLogs(flags, positional[0], *lines, stdout, stderr)
		case "drain":
			fs, flags := newFlagSet("runtime drain", stderr)
			reason := fs.String("reason", "", "reason recorded for the drain (default \"manual\")")
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printRuntimeUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime drain <node> [--reason=X] [flags]")
				return ExitUserError
			}
			return runRuntimeDrain(flags, positional[0], *reason, stdout, stderr)
		case "undrain":
			fs, flags := newFlagSet("runtime undrain", stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printRuntimeUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime undrain <node> [flags]")
				return ExitUserError
			}
			return runRuntimeUndrain(flags, positional[0], stdout, stderr)
		case "health":
			fs, flags := newFlagSet("runtime health", stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printRuntimeUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime health <node> [flags]")
				return ExitUserError
			}
			return runRuntimeHealth(flags, positional[0], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown runtime action %q (want start, stop, restart, logs, drain, undrain, or health)\n\n", action)
			printRuntimeUsage(stderr)
			return ExitUserError
		}
	case "node":
		if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
			printNodeControlUsage(stdout)
			return ExitOK
		}
		if len(rest) < 1 || rest[0] != "control" {
			printNodeControlUsage(stderr)
			return ExitUserError
		}
		if len(rest) < 2 || rest[1] == "-h" || rest[1] == "--help" {
			w := stderr
			code := ExitUserError
			if len(rest) >= 2 {
				w, code = stdout, ExitOK
			}
			printNodeControlUsage(w)
			return code
		}
		switch rest[1] {
		case "probe":
			fs, flags := newFlagSet("node control probe", stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[2:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printNodeControlUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh node control probe <node> [flags]")
				return ExitUserError
			}
			return runNodeControlProbe(flags, positional[0], stdout, stderr)
		case "accept":
			fs, flags := newFlagSet("node control accept", stderr)
			driver := fs.String("driver", "", "control driver: systemd, docker, process, launchd, or windows_service")
			identifier := fs.String("identifier", "", "driver-specific identifier (unit name, container name, PID file path, plist label, service name)")
			startCommand := fs.String("start-command", "", "launch command for the process driver's Start action (only meaningful when --driver=process)")
			flagArgs, positional := splitFlagsAndArgs(rest[2:], map[string]bool{"json": true})
			if ok, code := parseFlags(fs, flagArgs, printNodeControlUsage, stdout); !ok {
				return code
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh node control accept <node> --driver X --identifier Y [--start-command Z]")
				return ExitUserError
			}
			if *driver == "" || *identifier == "" {
				fmt.Fprintln(stderr, "error: --driver and --identifier are required")
				return ExitUserError
			}
			return runNodeControlAccept(flags, positional[0], *driver, *identifier, *startCommand, stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown node control action %q\n\n", rest[1])
			printNodeControlUsage(stderr)
			return ExitUserError
		}
	case "key":
		if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
			printKeyUsage(stdout)
			return ExitOK
		}
		if len(rest) < 1 || (rest[0] != "set-local-only" && rest[0] != "set-allow-local-degradation") {
			printKeyUsage(stderr)
			return ExitUserError
		}
		action := rest[0]
		fs, flags := newFlagSet("key "+action, stderr)
		flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
		if ok, code := parseFlags(fs, flagArgs, printKeyUsage, stdout); !ok {
			return code
		}
		if len(positional) != 2 {
			fmt.Fprintf(stderr, "usage: ollama-mesh key %s <name> <true|false>\n", action)
			return ExitUserError
		}
		if action == "set-allow-local-degradation" {
			return runKeySetAllowLocalDegradation(flags, positional[0], positional[1], stdout, stderr)
		}
		return runKeySetLocalOnly(flags, positional[0], positional[1], stdout, stderr)
	case "spill":
		fs, flags := newFlagSet("spill", stderr)
		if ok, code := parseFlags(fs, rest, nil, stdout); !ok {
			return code
		}
		return runSpill(flags, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return ExitUserError
	}
}
