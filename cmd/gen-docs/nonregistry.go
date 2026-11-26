package main

// nonregistry.go lists the subcommands of the marbor binary that are
// dispatched in cmd/marbor/main.go before internal/cli's registry is ever
// consulted ("bench" runs the benchmark tool, "uninstall" removes the
// service registration), so cli.Root() never sees them. They are real
// commands an operator can run, so the generated reference must cover them:
// this list is the one place gen-docs does not derive from the registry.
// Flag names, types, defaults and usage text must match each command's own
// flag declarations, and the usage lines must match the binary's help table;
// the drift tests in nonregistry_test.go fail when a command or flag is added
// or changed without this list following. The Marbor Agent is a separate
// binary (cmd/marbor-agent) and must never be listed here as a subcommand.

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

// nonRegistryCommands is in the order the commands are documented.
var nonRegistryCommands = []nonRegistryCommand{
	{
		Name:  "bench",
		Usage: "marbor bench [flags]",
		Short: "Benchmark warm-vs-cold first-token latency against a running marbor",
		Long: "Measures cold vs warm Time-To-First-Token (TTFT) through the marbor proxy. " +
			"--target must point at the marbor proxy port, not an Ollama backend directly. " +
			"It talks to the proxy with an API key, not to the Admin API, so the global " +
			"Admin API flags (--server, --json, --username, --password) do not apply to it.",
		Flags: []nonRegistryFlag{
			{Name: "target", Signature: "--target string", Usage: `Marbor proxy base URL (not the Ollama backend) (default "http://localhost:11434")`},
			{Name: "model", Signature: "--model string", Usage: "Model to benchmark (auto-detected from /v1/models if omitted)"},
			{Name: "key", Signature: "--key string", Usage: "Bearer API key (required if auth is enabled on the marbor)"},
			{Name: "json", Signature: "--json", Usage: "Emit JSON output instead of the human-readable table"},
			{Name: "timeout", Signature: "--timeout duration", Usage: "Per-request timeout (cold load can take minutes on a large model) (default 5m0s)"},
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
		Long: "Removes the marbor's own systemd service registration (if any) from this host and stops a " +
			"marbor started in the background by the installer (the process named in marbor.pid). " +
			"To remove a marbor agent, run \"marbor-agent service uninstall\" on the agent's own host instead. " +
			"Never deletes marbor.db - remove it yourself if you also want the database gone. " +
			"Run from the same directory the marbor server was started from, so a nohup-mode marbor.pid is found. " +
			"It acts on this host only, so the global Admin API flags (--server, --json, --username, --password) do not apply to it.",
		Flags: []nonRegistryFlag{
			{Name: "purge", Signature: "--purge", Usage: "also delete the installed binary (default: only removes service registrations)"},
		},
		Examples: []string{
			"marbor uninstall",
			"marbor uninstall --purge",
		},
	},
}
