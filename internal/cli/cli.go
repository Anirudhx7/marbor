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
  models pull <node> <model>                 start pulling a model onto a node (async - does not wait for completion)
  models delete <node> <model>               delete a model from a node's local storage
  models unload <node> <model>               unload a model from a node's warm state
  models list <node>                         list models present on a node's local storage
  runtime start|stop|restart <node>          start/stop/restart a node's inference runtime process
  runtime logs <node> [--lines=N]            fetch recent log lines from a node's runtime process
  runtime drain <node> [--reason=X]          mark a node draining (stop routing new requests to it)
  runtime undrain <node>                     reverse "runtime drain"
  runtime health <node>                      run an on-demand active liveness probe on a node
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
		if len(rest) > 0 {
			switch rest[0] {
			case "pull":
				fs, flags := newFlagSet("models pull", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if err := fs.Parse(flagArgs); err != nil {
					return ExitUserError
				}
				if len(positional) != 2 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models pull <node> <model> [flags]")
					return ExitUserError
				}
				return runModelsPull(flags, positional[0], positional[1], stdout, stderr)
			case "delete":
				fs, flags := newFlagSet("models delete", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if err := fs.Parse(flagArgs); err != nil {
					return ExitUserError
				}
				if len(positional) != 2 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models delete <node> <model> [flags]")
					return ExitUserError
				}
				return runModelsDelete(flags, positional[0], positional[1], stdout, stderr)
			case "unload":
				fs, flags := newFlagSet("models unload", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if err := fs.Parse(flagArgs); err != nil {
					return ExitUserError
				}
				if len(positional) != 2 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models unload <node> <model> [flags]")
					return ExitUserError
				}
				return runModelsUnload(flags, positional[0], positional[1], stdout, stderr)
			case "list":
				fs, flags := newFlagSet("models list", stderr)
				flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
				if err := fs.Parse(flagArgs); err != nil {
					return ExitUserError
				}
				if len(positional) != 1 {
					fmt.Fprintln(stderr, "usage: ollama-mesh models list <node> [flags]")
					return ExitUserError
				}
				return runModelsList(flags, positional[0], stdout, stderr)
			}
		}
		fs, flags := newFlagSet("models", stderr)
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runModels(flags, stdout, stderr)
	case "runtime":
		if len(rest) < 1 {
			fmt.Fprintln(stderr, "usage: ollama-mesh runtime <start|stop|restart|logs|drain|undrain|health> <node>")
			return ExitUserError
		}
		action := rest[0]
		switch action {
		case "start", "stop", "restart":
			fs, flags := newFlagSet("runtime "+action, stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
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
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
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
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime drain <node> [--reason=X] [flags]")
				return ExitUserError
			}
			return runRuntimeDrain(flags, positional[0], *reason, stdout, stderr)
		case "undrain":
			fs, flags := newFlagSet("runtime undrain", stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime undrain <node> [flags]")
				return ExitUserError
			}
			return runRuntimeUndrain(flags, positional[0], stdout, stderr)
		case "health":
			fs, flags := newFlagSet("runtime health", stderr)
			flagArgs, positional := splitFlagsAndArgs(rest[1:], map[string]bool{"json": true})
			if err := fs.Parse(flagArgs); err != nil {
				return ExitUserError
			}
			if len(positional) != 1 {
				fmt.Fprintln(stderr, "usage: ollama-mesh runtime health <node> [flags]")
				return ExitUserError
			}
			return runRuntimeHealth(flags, positional[0], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown runtime action %q (want start, stop, restart, logs, drain, undrain, or health)\n\n", action)
			return ExitUserError
		}
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
