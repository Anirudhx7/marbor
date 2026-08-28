package cli

import "sync"

// root is the CLI command tree, built exactly once on first use. Using
// sync.OnceValue (stdlib, no dependency) rather than an init()-time global
// or a package-level var literal means: (1) buildRoot only runs when
// something actually asks for the tree - never at server start, since the
// marbor server binary imports this package but never calls into CLI dispatch
// - and (2) tests can still call buildRoot() directly to get a fresh,
// unmemoized tree for constructing deliberately-malformed variants.
//
// This tree is UNUSED as of this plan step: nothing in cli.go, main.go, or
// any dispatcher reads root() yet. It exists purely as a data structure to
// be validated by finalize and, in later plan steps, wired into the real
// dispatcher, help writer, completion writer, man generator, and main.go's
// command whitelist.
var root = sync.OnceValue(buildRoot)

// rootBuilt is set by buildRoot and exists solely so
// TestRegistry_TreeValid can confirm the tree is not constructed eagerly
// (e.g. by an accidental init() call) - it is not read anywhere in
// non-test code.
var rootBuilt bool

// notYetMigrated is the placeholder Run for every leaf command in this plan
// step. finalize requires every leaf (Run == nil, no Sub) to have Run set;
// the alternative was relaxing that check to allow nil, but a hard panic
// here is a better guardrail - it means a later step that forgets to wire a
// leaf's real Run function fails loudly in TestRegistry_TreeValid instead of
// silently reaching an unreachable code path in production. Nothing calls
// Run yet (the dispatcher isn't wired), so this panic never fires outside
// of tests that would call it directly.
func notYetMigrated(ctx *RunCtx) int {
	panic("cli: Run not yet migrated for " + ctx.Cmd.Path())
}

// buildRoot constructs the full current CLI command tree, matching
// cli.go's switch-based dispatcher exactly as of this plan step: same
// names, same aliases (none exist today), same flags (name/kind/default/
// usage taken verbatim from the flag registrations and print*Usage
// functions in cli.go/keys.go/requests.go), and same positional arity
// (taken from the len(positional) != N checks). "key", "spill", and
// "requests" are included even though they are currently unreachable from
// the real binary (main.go's resolveCommand whitelist omits them) - fixing
// that reachability bug is a later plan step (item 2), not this one.
func buildRoot() *Command {
	rootBuilt = true
	authFlags := "requires credentials: run \"marbor login\" once (recommended), or pass --username+--password (or MARBOR_USERNAME+MARBOR_PASSWORD)."

	r := &Command{
		Name:  "marbor",
		Short: "CLI client for the Marbor Admin API",
		// Footer reproduces the credential-requirement note that used to be
		// hand-written at the bottom of cli.go's `usage` const (see Fix 3 of
		// the P83+ CLI hardening code review) - writeRootHelp prints it
		// verbatim below the global flags table, same as any group/leaf's
		// Footer.
		Footer: "\"nodes\", \"models\", \"runtime\", and \"node control\" require credentials: run\n" +
			"\"marbor login\" once (recommended), or pass --username+--password (or\n" +
			"MARBOR_USERNAME+MARBOR_PASSWORD) on every invocation instead.\n" +
			"\"version\" and \"status\" do not require credentials.",
		Sub: []*Command{
			{
				Name:  "version",
				Short: "print CLI and (if reachable) server version",
				Run:   func(ctx *RunCtx) int { return runVersion(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:  "status",
				Short: "print marbor health/status summary",
				Run:   func(ctx *RunCtx) int { return runStatus(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:  "login",
				Short: "authenticate once and save the session locally (recommended)",
				// Line breaks below are deliberate, not stylistic: they
				// reproduce printLoginUsage's old hand-wrapped paragraph
				// byte-for-byte (see testdata/help/login.golden) now that
				// help.go's writeHelp prints Long verbatim.
				Long: "Authenticates once and saves the resulting session to a local file (0600,\n" +
					"under the OS user config dir) so other commands can omit --username/\n" +
					"--password afterward. Run without --username/--password in a terminal to\n" +
					"be prompted interactively (password input is not echoed).",
				Run: func(ctx *RunCtx) int { return runLogin(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:  "logout",
				Short: "remove the saved session",
				Run:   func(ctx *RunCtx) int { return runLogout(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:  "whoami",
				Short: "show the CLI's saved identity (live-verified)",
				Run:   func(ctx *RunCtx) int { return runWhoami(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:      "nodes",
				Short:     "list nodes known to marbor",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runNodes(ctx.Flags, ctx.Stdout, ctx.Stderr) },
				Sub: []*Command{
					{
						Name:      "confirm-tls",
						Short:     "pin a marbor agent's TLS certificate fingerprint (headless enrollment)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Flags: []FlagSpec{
							{
								Name:        "fingerprint",
								Kind:        FlagString,
								Usage:       "SHA-256 fingerprint the operator has independently confirmed matches the node's actual TLS certificate (see \"agent service status\" on the node), in the form SHA256:<64 hex characters>",
								Required:    true,
								RequiredMsg: "error: --fingerprint is required (the value must come from you, not be silently accepted from the wire)",
							},
						},
						Run: func(ctx *RunCtx) int {
							return runNodesConfirmTLS(ctx.Flags, ctx.Args[0], ctx.String("fingerprint"), ctx.Stdout, ctx.Stderr)
						},
					},
				},
			},
			{
				Name:      "models",
				Short:     "fleet-wide list, or pull/delete/unload/list on one node",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runModels(ctx.Flags, ctx.Stdout, ctx.Stderr) },
				Sub: []*Command{
					{
						Name:      "pull",
						Short:     "start pulling a model onto a node (async - does not wait for completion)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}, {Name: "model"}},
						Run: func(ctx *RunCtx) int {
							return runModelsPull(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "delete",
						Short:     "delete a model from a node's local storage",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}, {Name: "model"}},
						Run: func(ctx *RunCtx) int {
							return runModelsDelete(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "unload",
						Short:     "unload a model from a node's warm state",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}, {Name: "model"}},
						Run: func(ctx *RunCtx) int {
							return runModelsUnload(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "list",
						Short:     "list models present on a node's local storage (per-node, not the fleet-wide aggregate above)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Run:       func(ctx *RunCtx) int { return runModelsList(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "fleet",
						Short:     "fleet residency with VRAM totals and drift (same live data as bare models, filterable)",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "drifted-only", Kind: FlagBool, Usage: "only show models where nodes disagree on digest"},
						},
						Run: func(ctx *RunCtx) int {
							return runModelsFleet(ctx.Flags, ctx.Bool("drifted-only"), ctx.Stdout, ctx.Stderr)
						},
					},
				},
			},
			{
				Name:      "runtime",
				Short:     "start/stop/restart/logs/drain/undrain/health on one node",
				NeedsAuth: true,
				Footer:    authFlags,
				Long: "\"start|stop|restart\" requires the target node to have an operator-accepted " +
					"control driver (see \"node control accept\") - a node with none configured " +
					"returns an error rather than guessing one.\n\n" +
					"\"logs\" is a point-in-time snapshot, not a live tail. A node whose control " +
					"driver has no real log source (e.g. a bare PID-file process with no supervisor) " +
					"returns a clear \"not supported\" error.",
				Sub: []*Command{
					{Name: "start", Short: "start the node's inference runtime process", NeedsAuth: true, Args: []ArgSpec{{Name: "node"}}, Run: func(ctx *RunCtx) int {
						return runRuntimeAction(ctx.Flags, ctx.Cmd.Name, ctx.Args[0], ctx.Stdout, ctx.Stderr)
					}},
					{Name: "stop", Short: "stop the node's inference runtime process", NeedsAuth: true, Args: []ArgSpec{{Name: "node"}}, Run: func(ctx *RunCtx) int {
						return runRuntimeAction(ctx.Flags, ctx.Cmd.Name, ctx.Args[0], ctx.Stdout, ctx.Stderr)
					}},
					{Name: "restart", Short: "restart the node's inference runtime process", NeedsAuth: true, Args: []ArgSpec{{Name: "node"}}, Run: func(ctx *RunCtx) int {
						return runRuntimeAction(ctx.Flags, ctx.Cmd.Name, ctx.Args[0], ctx.Stdout, ctx.Stderr)
					}},
					{
						Name:      "logs",
						Short:     "fetch recent log lines from the node's runtime process",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Flags: []FlagSpec{
							{Name: "lines", Kind: FlagInt, DefInt: 0, Usage: "number of log lines to fetch (0 = server default)"},
						},
						Run: func(ctx *RunCtx) int {
							return runRuntimeLogs(ctx.Flags, ctx.Args[0], ctx.Int("lines"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "drain",
						Short:     "mark the node draining (stop routing new requests to it)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Flags: []FlagSpec{
							{Name: "reason", Kind: FlagString, DefString: "", Usage: `reason recorded for the drain (default "manual")`},
						},
						Run: func(ctx *RunCtx) int {
							return runRuntimeDrain(ctx.Flags, ctx.Args[0], ctx.String("reason"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "undrain",
						Short:     `reverse "runtime drain"`,
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Run:       func(ctx *RunCtx) int { return runRuntimeUndrain(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "health",
						Short:     "run an on-demand active liveness probe on the node",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Run:       func(ctx *RunCtx) int { return runRuntimeHealth(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:  "node",
				Short: "node control driver operations",
				Sub: []*Command{
					{
						// NeedsAuth is set on this group (not just its
						// children) so writeHelp's group-help shape prints
						// authFlagsRows - matching printNodeControlUsage's
						// old output, which always showed the auth flags
						// regardless of which action was requested.
						Name:      "control",
						Short:     "show or accept a node's control driver",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "probe",
								Short:     "show a node's control-driver status (configured + discovered)",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Run:       func(ctx *RunCtx) int { return runNodeControlProbe(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "accept",
								Short:     "accept a control driver + identifier for a node",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									// driver/identifier share one combined RequiredMsg (not
									// per-flag wording) because that is what cli.go:517-520's
									// old check produced regardless of which of the two was
									// actually left empty - see dispatch.go's required-flag
									// validation, which prints whichever zero flag it hits
									// first.
									{Name: "driver", Kind: FlagString, Usage: "control driver: systemd, docker, process, launchd, or windows_service", Required: true, RequiredMsg: "error: --driver and --identifier are required"},
									{Name: "identifier", Kind: FlagString, Usage: "driver-specific identifier (unit name, container name, PID file path, plist label, service name)", Required: true, RequiredMsg: "error: --driver and --identifier are required"},
									{Name: "start-command", Kind: FlagString, Usage: "launch command for the process driver's Start action (only meaningful when --driver=process)"},
								},
								Run: func(ctx *RunCtx) int {
									return runNodeControlAccept(ctx.Flags, ctx.Args[0], ctx.String("driver"), ctx.String("identifier"), ctx.String("start-command"), ctx.Stdout, ctx.Stderr)
								},
							},
						},
					},
				},
			},
			{
				// NeedsAuth is set on this group (not just its children)
				// so writeHelp's group-help shape prints authFlagsRows -
				// matching printKeyUsage's old output.
				Name:      "key",
				Short:     "per-API-key local/cloud routing overrides",
				NeedsAuth: true,
				Sub: []*Command{
					{
						Name:      "set-local-only",
						Short:     "block (or re-allow) cloud fallback for one API key",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "name"}, {Name: "true|false"}},
						Run: func(ctx *RunCtx) int {
							return runKeySetLocalOnly(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "set-allow-local-degradation",
						Short:     "let (or forbid) one API key receive a local alternate model",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "name"}, {Name: "true|false"}},
						Run: func(ctx *RunCtx) int {
							return runKeySetAllowLocalDegradation(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
						},
					},
				},
			},
			{
				Name:      "spill",
				Short:     "show per-key, per-provider local-vs-cloud request counts",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runSpill(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:      "activity",
				Short:     "show unified fleet activity feed (drain, agent, runtime, node, warmup, schedule, predictive, config)",
				Long:      "Times are shown in UTC (RFC3339 Z) — the Admin API stores every audit event in UTC. The dashboard renders the same instants in the operator's configured timezone; this CLI shows the raw UTC value.",
				NeedsAuth: true,
				Footer:    authFlags,
				Flags: []FlagSpec{
					{Name: "limit", Kind: FlagInt, DefInt: 100, Usage: "max events to show (1-200, default 100)"},
					{Name: "kind", Kind: FlagString, DefString: "", Usage: "filter by kind: drain, agent, runtime, node, warmup, schedule, predictive, config, or all (default all)"},
					{Name: "from", Kind: FlagString, DefString: "", Usage: "filter from time (RFC3339, e.g. 2026-08-26T00:00:00Z)"},
					{Name: "to", Kind: FlagString, DefString: "", Usage: "filter to time (RFC3339, e.g. 2026-08-26T23:59:59Z)"},
					{Name: "before", Kind: FlagString, DefString: "", Usage: "paginate before time (RFC3339, exclusive)"},
					{Name: "action", Kind: FlagString, DefString: "", Usage: "filter by exact action (e.g. drain_node)"},
					{Name: "user", Kind: FlagString, DefString: "", Usage: "filter by operator username (prefix match)"},
					{Name: "target", Kind: FlagString, DefString: "", Usage: "filter by target (substring, e.g. gpu-node-02)"},
					{Name: "source_ip", Kind: FlagString, DefString: "", Usage: "filter by source IP (substring)"},
				},
				Run: func(ctx *RunCtx) int {
					return runActivity(ctx.Flags, ctx.Int("limit"), ctx.String("kind"), ctx.String("from"), ctx.String("to"), ctx.String("action"), ctx.String("user"), ctx.String("target"), ctx.String("source_ip"), ctx.String("before"), ctx.Stdout, ctx.Stderr)
				},
			},
			{
				// NeedsAuth is set on this group (not just its children)
				// so writeHelp's group-help shape prints authFlagsRows -
				// matching printRequestsUsage's old output.
				Name:      "requests",
				Short:     "inspect routing decisions for past requests",
				NeedsAuth: true,
				Sub: []*Command{
					{
						Name:      "explain",
						Short:     "show why the router picked the node it did for one request",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "request-id"}},
						Run:       func(ctx *RunCtx) int { return runRequestsExplain(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			completionCmd(),
		},
	}

	return finalize(r)
}

// completionCmd returns the "completion" top-level command node. It is
// Hidden (omitted from the root command table generated by writeRootHelp/the
// hand-written usage const) but still fully reachable via dispatch:
// Command.lookup (registry.go) matches by Name/Aliases with no Hidden check,
// so "marbor completion bash" walks and dispatches exactly like any
// other command - Hidden means "not advertised", never "unreachable". Its
// Run (runCompletion, completion.go) is pure and local: the generated script
// is baked from the current registry tree at generation time, so it never
// contacts marbor or requires credentials (plan Implementation section 7).
func completionCmd() *Command {
	return &Command{
		Name:  "completion",
		Short: "generate a shell completion script (bash, zsh, or fish)",
		Long: "Generates a static completion script for the requested shell by walking\n" +
			"the current command tree. The script never contacts marbor or\n" +
			"requires credentials, so it keeps working even when marbor is\n" +
			"unreachable or the operator isn't logged in.",
		Hidden: true,
		Args:   []ArgSpec{{Name: "shell"}},
		Examples: []string{
			`source <(marbor completion bash)`,
			`marbor completion zsh > "${fpath[1]}/_marbor"`,
			`marbor completion fish > ~/.config/fish/completions/marbor.fish`,
		},
		Run: runCompletion,
	}
}
