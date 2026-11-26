package main

// nonregistry.go lists the subcommands of the marbor binary that are
// dispatched in cmd/marbor/main.go before internal/cli's registry is ever
// consulted ("bench" runs the benchmark tool, "uninstall" removes the
// service registration), so cli.Root() never sees them. They are real
// commands an operator can run, so the generated reference must cover them:
// this list is the one place gen-docs does not derive from the registry. Text
// here is copied from each command's own --help output; the drift tests in
// nonregistry_test.go fail when a command, or one of its flags, is added to
// the binary without being listed here.

// nonRegistryFlag is one flag of a non-registry command. Signature is the
// flag as shown to the operator, e.g. "--target string".
type nonRegistryFlag struct {
	Name      string
	Signature string
	Usage     string
}

// nonRegistryCommand describes one non-registry subcommand.
type nonRegistryCommand struct {
	Name     string
	Usage    string // the usage line, e.g. "marbor uninstall [--purge]"
	Short    string
	Long     string
	Flags    []nonRegistryFlag
	Examples []string
}

var nonRegistryCommands = []nonRegistryCommand{
	{
		Name:  "bench",
		Usage: "marbor bench [flags]",
		Short: "Benchmark warm-vs-cold first-token latency against a running marbor",
		Long: "Measures cold vs warm Time-To-First-Token (TTFT) through the marbor proxy. " +
			"--target must point at the marbor proxy port, not an Ollama backend directly. " +
			"It talks to the proxy with an API key, not to the Admin API, so the global " +
			"flags above do not apply to it.",
		Flags: []nonRegistryFlag{
			{"target", "--target string", `Marbor proxy base URL (not the Ollama backend) (default "http://localhost:11434")`},
			{"model", "--model string", "Model to benchmark (auto-detected from /v1/models if omitted)"},
			{"key", "--key string", "Bearer API key (required if auth is enabled on the marbor)"},
			{"json", "--json", "Emit JSON output instead of the human-readable table"},
			{"timeout", "--timeout duration", "Per-request timeout (cold load can take minutes on a large model) (default 5m0s)"},
		},
		Examples: []string{
			"marbor bench --target http://localhost:11434 --model llama3:8b",
			"marbor bench --json --key <api-key>",
		},
	},
	{
		Name:  "uninstall",
		Usage: "marbor uninstall [--purge]",
		Short: "Remove the marbor's own service registration from this host",
		Long: "Removes the marbor's own service registration (if any) from this host. " +
			"To remove a marbor agent, run \"marbor-agent service uninstall\" on the agent's own host instead. " +
			"Never deletes marbor.db - remove it yourself if you also want the database gone. " +
			"Run from the same directory the marbor server was started from, so a nohup-mode marbor.pid is found. " +
			"It acts on this host only, so the global flags above do not apply to it.",
		Flags: []nonRegistryFlag{
			{"purge", "--purge", "also delete the installed binary (default: only removes service registrations)"},
		},
		Examples: []string{
			"marbor uninstall",
			"marbor uninstall --purge",
		},
	},
}
