package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
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

func newFlagSet(name string) (*flag.FlagSet, *globalFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	g := &globalFlags{}
	fs.StringVar(&g.server, "server", envOr("MESH_SERVER", "http://localhost:8080"), "Admin API base URL")
	fs.BoolVar(&g.jsonOutput, "json", false, "output machine-readable JSON instead of a human table")
	fs.StringVar(&g.token, "token", os.Getenv("MESH_TOKEN"), "session token (Authorization: Bearer)")
	fs.StringVar(&g.username, "username", os.Getenv("MESH_USERNAME"), "admin username, used to log in if --token is not set")
	fs.StringVar(&g.password, "password", os.Getenv("MESH_PASSWORD"), "admin password, used to log in if --token is not set")
	return fs, g
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

const usage = `mesh - CLI client for the ollama-mesh Admin API

Usage:
  mesh <command> [flags]

Commands:
  version   print CLI and (if reachable) server version
  status    print mesh health/status summary
  nodes     list nodes known to the mesh
  models    list models known across the fleet

Global flags:
  --server string      Admin API base URL (default "http://localhost:8080", env MESH_SERVER)
  --json                output machine-readable JSON instead of a human table
  --token string        session token for authenticated commands (env MESH_TOKEN)
  --username string     admin username, used to log in if --token is unset (env MESH_USERNAME)
  --password string      admin password, used to log in if --token is unset (env MESH_PASSWORD)

"nodes" and "models" require credentials (--token, or --username/--password).
"version" and "status" do not.
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
		fs, flags := newFlagSet("version")
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runVersion(flags, stdout, stderr)
	case "status":
		fs, flags := newFlagSet("status")
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runStatus(flags, stdout, stderr)
	case "nodes":
		fs, flags := newFlagSet("nodes")
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runNodes(flags, stdout, stderr)
	case "models":
		fs, flags := newFlagSet("models")
		if err := fs.Parse(rest); err != nil {
			return ExitUserError
		}
		return runModels(flags, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return ExitUserError
	}
}
