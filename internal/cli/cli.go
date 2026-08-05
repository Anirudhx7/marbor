package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// globalFlags holds the flags every subcommand accepts, per
// operational-interfaces.md 5.1/5.2: --json is the compatibility contract
// from the first command shipped, auth flags are shared across commands.
type globalFlags struct {
	server     string
	jsonOutput bool
	token      string
	username   string
	password   string
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *globalFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := &globalFlags{}
	fs.StringVar(&g.server, "server", envOr("MESH_SERVER", "http://localhost:8080"), "Admin API base URL")
	fs.BoolVar(&g.jsonOutput, "json", false, "output machine-readable JSON instead of a human table")
	fs.StringVar(&g.token, "token", os.Getenv("MESH_TOKEN"), "session token (Authorization: Bearer)")
	fs.StringVar(&g.username, "username", os.Getenv("MESH_USERNAME"), "admin username, used to log in if --token is not set")
	fs.StringVar(&g.password, "password", os.Getenv("MESH_PASSWORD"), "admin password, used to log in if --token is not set")
	return fs, g
}

// splitFlagsAndArgs separates args into flag tokens and positional
// arguments, tolerating either order (e.g. both "restart gpu-0 --token x"
// and "restart --token x gpu-0" work) - Go's flag.FlagSet only supports
// "flags then positional args" natively (it stops parsing at the first
// non-flag token), which is too strict for "mesh runtime restart <node>
// [flags]" where a human naturally types the node name first. boolFlagNames
// lists flags (without leading dashes) that never consume a following
// value token, e.g. "json" - every flag not in the set is treated as
// taking a value (either "--name=value" or "--name value").
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
// auth error (4) - auth was never attempted against the server.
func authenticatedClient(flags *globalFlags) (*Client, error) {
	if flags.token != "" {
		return NewClient(flags.server, flags.token), nil
	}
	if flags.username == "" || flags.password == "" {
		return nil, userErrorf("authentication required: pass --token, or --username/--password (or MESH_TOKEN / MESH_USERNAME+MESH_PASSWORD)")
	}
	client := NewClient(flags.server, "")
	if err := client.Login(flags.username, flags.password); err != nil {
		return nil, err
	}
	return client, nil
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
  version                          print CLI and (if reachable) server version
  status                           print mesh health/status summary
  nodes                            list nodes known to the mesh
  models                           list models known across the fleet
  runtime start|stop|restart <node>          start/stop/restart a node's inference runtime process
  runtime logs <node> [--lines=N]            fetch recent log lines from a node's runtime process
  node control probe <node>                  show a node's control-driver status (configured + discovered)
  node control accept <node> --driver X --identifier Y [--start-command Z]
                                              accept a control driver + identifier for a node

Global flags:
  --server string      Admin API base URL (default "http://localhost:8080", env MESH_SERVER)
  --json                output machine-readable JSON instead of a human table
  --token string        session token for authenticated commands (env MESH_TOKEN)
  --username string     admin username, used to log in if --token is unset (env MESH_USERNAME)
  --password string      admin password, used to log in if --token is unset (env MESH_PASSWORD)

"nodes", "models", "runtime", and "node control" require credentials
(--token, or --username/--password). "version" and "status" do not.

"runtime start|stop|restart" requires the target node to have an
operator-accepted control driver (see "node control accept") - a node with
none configured returns an error rather than guessing one.

"runtime logs" is a point-in-time snapshot, not a live tail. A node whose
control driver has no real log source (e.g. a bare PID-file process with no
supervisor) returns a clear "not supported" error.
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
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runVersion(flags, stdout, stderr)
	case "status":
		fs, flags := newFlagSet("status", stderr)
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runStatus(flags, stdout, stderr)
	case "nodes":
		fs, flags := newFlagSet("nodes", stderr)
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runNodes(flags, stdout, stderr)
	case "models":
		fs, flags := newFlagSet("models", stderr)
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runModels(flags, stdout, stderr)
	case "runtime":
		if len(rest) < 1 {
			fmt.Fprintln(stderr, "usage: ollama-mesh runtime <start|stop|restart|logs> <node>")
			return ExitUserError
		}
		action := rest[0]
		if action != "start" && action != "stop" && action != "restart" && action != "logs" {
			fmt.Fprintf(stderr, "unknown runtime action %q (want start, stop, restart, or logs)\n\n", action)
			return ExitUserError
		}
		fs, flags := newFlagSet("runtime "+action, stderr)
		if action == "logs" {
			lines := fs.Int("lines", 0, "number of log lines to fetch (0 = server default)")
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime logs <node> [--lines=N] [flags]")
				return ExitUserError
			}
			return runRuntimeLogs(flags, positional[0], *lines, stdout, stderr)
		}
		flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
		if err := fs.Parse(flagArgs); err != nil {
			return ExitUserError
		}
		if len(positional) != 1 {
			fmt.Fprintf(stderr, "usage: ollama-mesh runtime %s <node> [flags]\n", action)
			return ExitUserError
		}
		return runRuntimeAction(flags, action, positional[0], stdout, stderr)
	case "node":
		if len(rest) < 1 || rest[0] != "control" {
			fmt.Fprintln(stderr, "usage: ollama-mesh node control <probe|accept> <node> [flags]")
			return ExitUserError
		}
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: ollama-mesh node control <probe|accept> <node> [flags]")
			return ExitUserError
		}
		switch rest[1] {
		case "probe":
			fs, flags := newFlagSet("node control probe", stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[2:], map[string]bool{"json": true})
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
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
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
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
			return ExitUserError
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return ExitUserError
	}
}
