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
					{
						Name:      "patch",
						Short:     "set deployment parallelism or per-model VRAM overrides for a node",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Flags: []FlagSpec{
							{Name: "parallelism-type", Kind: FlagString, Usage: "parallelism type: tp, pp, ep, dp (empty to clear)"},
							{Name: "parallelism-width", Kind: FlagInt, Usage: "parallelism width 1..64 (0 to clear)"},
							{Name: "vram-override", Kind: FlagString, Usage: "per-model VRAM size overrides in MB, comma-separated model=mb pairs - REPLACES the whole declared list, dropping any entry not listed here (empty to clear all)"},
						},
						Run: func(ctx *RunCtx) int {
							return runNodesPatchWithCtx(ctx, ctx.Args[0])
						},
					},
					{
						Name:      "add",
						Short:     "add (or update, by name) a node in the fleet",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "name"}, {Name: "url"}},
						Flags: []FlagSpec{
							{Name: "runtime", Kind: FlagString, DefString: "", Usage: "runtime: ollama (default), vllm, tgi, llamacpp, mlx"},
							{Name: "gpu-model", Kind: FlagString, DefString: "", Usage: "GPU model label (informational)"},
							{Name: "vram-total-mb", Kind: FlagInt, DefInt: 0, Usage: "declared total VRAM in MB (0 = unknown)"},
						},
						Run: func(ctx *RunCtx) int {
							return runNodesAdd(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.String("gpu-model"), ctx.String("runtime"), int64(ctx.Int("vram-total-mb")), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "remove",
						Short:     "remove a node from the fleet",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}},
						Flags: []FlagSpec{
							{Name: "yes", Kind: FlagBool, Usage: "confirm removal without prompting"},
						},
						Run: func(ctx *RunCtx) int {
							return runNodesRemove(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "warmup",
						Short:     "get or set a node's proactive warmup config",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "get",
								Short:     "show a node's proactive warmup config",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Run:       func(ctx *RunCtx) int { return runNodesWarmupGet(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "set",
								Short:     "set a node's proactive warmup config",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									{Name: "enabled", Kind: FlagBool, Usage: "enable proactive warmup (omit to leave the node's current setting unchanged)"},
									{Name: "models", Kind: FlagString, DefString: "", Usage: "comma-separated models to keep resident (omit to leave unchanged, pass empty string to clear)"},
								},
								Run: func(ctx *RunCtx) int {
									return runNodesWarmupSet(ctx, ctx.Args[0])
								},
							},
						},
					},
					{
						Name:      "pinned",
						Short:     "get or set a node's never-evict (pinned) model list",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "get",
								Short:     "show a node's pinned model list",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Run:       func(ctx *RunCtx) int { return runNodesPinnedGet(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "set",
								Short:     "set a node's pinned model list (whole-list replace)",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									{Name: "models", Kind: FlagString, DefString: "", Usage: "comma-separated models to pin (empty clears all)"},
								},
								Run: func(ctx *RunCtx) int {
									return runNodesPinnedSet(ctx.Flags, ctx.Args[0], ctx.String("models"), ctx.Stdout, ctx.Stderr)
								},
							},
						},
					},
					{
						Name:      "prewarm",
						Short:     "disable or re-enable predictive prewarm for a node",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "set",
								Short:     "disable or re-enable predictive prewarm for a node",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									{Name: "disabled", Kind: FlagBool, Usage: "disable predictive prewarm for this node"},
								},
								Run: func(ctx *RunCtx) int {
									return runNodesPrewarmSet(ctx.Flags, ctx.Args[0], ctx.Bool("disabled"), ctx.Stdout, ctx.Stderr)
								},
							},
						},
					},
					{
						Name:      "fit",
						Short:     "show per-node VRAM fit analysis for resident/warm models",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runNodesFit(ctx.Flags, ctx.Stdout, ctx.Stderr) },
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
					{
						Name:      "search",
						Short:     "search Hugging Face models",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "q", Kind: FlagString, DefString: "", Usage: "search query"},
							{Name: "runtime", Kind: FlagString, DefString: "", Usage: "filter by runtime compatibility"},
							{Name: "sort", Kind: FlagString, DefString: "", Usage: "downloads (default), likes, newest, or oldest"},
						},
						Run: func(ctx *RunCtx) int {
							return runModelsSearch(ctx.Flags, ctx.String("q"), ctx.String("runtime"), ctx.String("sort"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "repo",
						Short:     "show Hugging Face repo detail with per-node fit",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "owner/name"}},
						Flags: []FlagSpec{
							{Name: "node", Kind: FlagString, DefString: "", Usage: "node to check fit/downloaded-status against"},
							{Name: "runtime", Kind: FlagString, DefString: "", Usage: "runtime to size variants for"},
							{Name: "ctx", Kind: FlagInt, DefInt: 0, Usage: "context window in tokens for VRAM sizing (0 = server default 8192)"},
						},
						Run: func(ctx *RunCtx) int {
							return runModelsRepo(ctx.Flags, ctx.Args[0], ctx.String("node"), ctx.String("runtime"), ctx.Int("ctx"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "pull-progress",
						Short:     "show a point-in-time snapshot of an active pull",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}, {Name: "model"}},
						Run: func(ctx *RunCtx) int {
							return runModelsPullProgress(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "cancel-pull",
						Short:     "cancel an in-flight pull",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}, {Name: "model"}},
						Run: func(ctx *RunCtx) int {
							return runModelsCancelPull(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Stdout, ctx.Stderr)
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
							{
								Name:      "clear",
								Short:     "clear the accepted control driver for a node",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									{Name: "yes", Kind: FlagBool, Usage: "confirm without prompting"},
								},
								Run: func(ctx *RunCtx) int {
									return runNodeControlClear(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
								},
							},
						},
					},
					{
						// Deliberately NOT a bare top-level "agent" command: main.go's
						// resolveCommand special-cases the literal word "agent" to mean
						// "removed" (the marbor agent binary split, v0.19.2) before it ever
						// consults this registry - see CLAUDE.md's "marbor agent ... no
						// longer exists" note and TestResolveCommand_MatchesRegistry, which
						// caught this the first time (P-A2-09b). Nested here as a sibling of
						// "control" instead, matching the existing "node control ..." shape.
						Name:      "agent",
						Short:     "manage marbor agent lifecycle for a node",
						NeedsAuth: true,
						Footer:    authFlags,
						Sub: []*Command{
							{
								Name:      "get",
								Short:     "show a node's marbor agent config (does not display the auth token)",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Run:       func(ctx *RunCtx) int { return runAgentGet(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "enable",
								Short:     "enable or reconfigure the marbor agent for a node",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									{Name: "port", Kind: FlagInt, Usage: "agent port (required)", Required: true, RequiredMsg: "error: --port is required"},
									{Name: "scheme", Kind: FlagString, DefString: "", Usage: "http or https (empty = keep existing, or http on first enable)"},
								},
								Run: func(ctx *RunCtx) int {
									return runAgentEnable(ctx.Flags, ctx.Args[0], ctx.Int("port"), ctx.String("scheme"), ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "disable",
								Short:     "disable the marbor agent for a node",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Flags: []FlagSpec{
									{Name: "yes", Kind: FlagBool, Usage: "confirm without prompting"},
								},
								Run: func(ctx *RunCtx) int {
									return runAgentDisable(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "regenerate",
								Short:     "issue a fresh token for an already-enabled marbor agent",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "node"}},
								Run:       func(ctx *RunCtx) int { return runAgentRegenerate(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
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
				Short:     "per-API-key local/cloud routing overrides (masked list, plaintext-once on create)",
				NeedsAuth: true,
				Sub: []*Command{
					{
						Name:      "list",
						Short:     "list keys (masked)",
						NeedsAuth: true,
						Run: func(ctx *RunCtx) int {
							return runKeyList(ctx.Flags, ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "create",
						Short:     "create a key (prints plaintext once)",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "name", Kind: FlagString, Usage: "key name (required)", Required: true, RequiredMsg: "error: --name is required"},
							{Name: "rate-limit", Kind: FlagInt, Usage: "max requests per hour (0 = unlimited)"},
							{Name: "daily-limit", Kind: FlagInt, Usage: "max requests per day (0 = unlimited)"},
							{Name: "monthly-limit", Kind: FlagInt, Usage: "max requests per month (0 = unlimited)"},
							{Name: "daily-usd-cap", Kind: FlagString, Usage: "daily cloud spend cap in USD (0 = unlimited)"},
							{Name: "monthly-usd-cap", Kind: FlagString, Usage: "monthly cloud spend cap in USD (0 = unlimited)"},
							{Name: "models", Kind: FlagString, Usage: "comma-separated allowed models (empty = all)"},
							{Name: "expires-at", Kind: FlagString, Usage: "expiry date (2006-01-02 or RFC3339)"},
							{Name: "key", Kind: FlagString, Usage: "explicit secret (default: server-generated)"},
							{Name: "local-only", Kind: FlagString, Usage: "block cloud fallback: true or false"},
							{Name: "allow-local-degradation", Kind: FlagString, Usage: "allow local alternate model: true or false"},
						},
						Run: func(ctx *RunCtx) int {
							return runKeyCreate(ctx.Flags, ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "revoke",
						Short:     "revoke (delete) a key",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "name"}},
						Flags: []FlagSpec{
							{Name: "yes", Kind: FlagBool, Usage: "confirm revocation without prompting"},
						},
						Run: func(ctx *RunCtx) int {
							return runKeyRevoke(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "patch",
						Short:     "update key settings",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "name"}},
						Flags: []FlagSpec{
							{Name: "rate-limit", Kind: FlagString, Usage: "max requests per hour (0 = unlimited)"},
							{Name: "daily-limit", Kind: FlagString, Usage: "max requests per day (0 = unlimited)"},
							{Name: "monthly-limit", Kind: FlagString, Usage: "max requests per month (0 = unlimited)"},
							{Name: "daily-usd-cap", Kind: FlagString, Usage: "daily cloud spend cap in USD"},
							{Name: "monthly-usd-cap", Kind: FlagString, Usage: "monthly cloud spend cap in USD"},
							{Name: "models", Kind: FlagString, Usage: "comma-separated allowed models (empty = clear)"},
							{Name: "expires-at", Kind: FlagString, Usage: "expiry date (2006-01-02 or RFC3339, empty = clear)"},
							{Name: "local-only", Kind: FlagString, Usage: "block cloud fallback: true or false"},
							{Name: "allow-local-degradation", Kind: FlagString, Usage: "allow local alternate model: true or false"},
						},
						Run: func(ctx *RunCtx) int {
							return runKeyPatch(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr, ctx)
						},
					},
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
				Name:      "schedules",
				Short:     "manage time-of-day warmup/unload/drain/undrain automations",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "list",
						Short:     "list schedules",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runSchedulesList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "create",
						Short:     "create a schedule",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "action", Kind: FlagString, Usage: "warmup, unload, drain, or undrain (required)", Required: true, RequiredMsg: "error: --action is required"},
							{Name: "node", Kind: FlagString, Usage: "target node name (required)", Required: true, RequiredMsg: "error: --node is required"},
							{Name: "at", Kind: FlagString, Usage: "time of day, HH:MM 24h server-local (required)", Required: true, RequiredMsg: "error: --at is required"},
							{Name: "models", Kind: FlagString, DefString: "", Usage: "comma-separated models (required for warmup/unload)"},
							{Name: "days", Kind: FlagString, DefString: "", Usage: "comma-separated days 0=Sun..6=Sat (empty = every day)"},
							{Name: "enabled", Kind: FlagBool, Usage: "enable immediately"},
						},
						Run: func(ctx *RunCtx) int {
							return runSchedulesCreate(ctx.Flags, ctx.String("action"), ctx.String("node"), ctx.String("at"), ctx.String("models"), ctx.String("days"), ctx.Bool("enabled"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "patch",
						Short:     "update a schedule (only flags you pass are changed)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Flags: []FlagSpec{
							{Name: "enabled", Kind: FlagBool, Usage: "enable or disable"},
							{Name: "action", Kind: FlagString, Usage: "warmup, unload, drain, or undrain"},
							{Name: "node", Kind: FlagString, Usage: "target node name"},
							{Name: "models", Kind: FlagString, Usage: "comma-separated models (empty clears)"},
							{Name: "at", Kind: FlagString, Usage: "time of day, HH:MM 24h server-local"},
							{Name: "days", Kind: FlagString, Usage: "comma-separated days 0=Sun..6=Sat (empty = every day)"},
						},
						Run: func(ctx *RunCtx) int { return runSchedulesPatch(ctx, ctx.Args[0]) },
					},
					{
						Name:      "delete",
						Short:     "delete a schedule",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Flags: []FlagSpec{
							{Name: "yes", Kind: FlagBool, Usage: "confirm deletion without prompting"},
						},
						Run: func(ctx *RunCtx) int {
							return runSchedulesDelete(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
						},
					},
				},
			},
			{
				Name:      "routing",
				Short:     "manage routing rules and global routing strategy",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "rules",
						Short:     "list/add/remove/toggle routing rules",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "list",
								Short:     "list routing rules",
								NeedsAuth: true,
								Run:       func(ctx *RunCtx) int { return runRoutingRulesList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "add",
								Short:     "add a routing rule",
								NeedsAuth: true,
								Flags: []FlagSpec{
									{Name: "id", Kind: FlagString, Usage: "rule id (required)", Required: true, RequiredMsg: "error: --id is required"},
									{Name: "condition", Kind: FlagString, Usage: "match condition (required)", Required: true, RequiredMsg: "error: --condition is required"},
									{Name: "target", Kind: FlagString, DefString: "", Usage: "target node name"},
									{Name: "strategy", Kind: FlagString, DefString: "", Usage: "per-rule strategy override"},
									{Name: "priority", Kind: FlagInt, DefInt: 0, Usage: "rule priority (higher wins)"},
									{Name: "enabled", Kind: FlagBool, Usage: "enable immediately"},
								},
								Run: func(ctx *RunCtx) int {
									return runRoutingRulesAdd(ctx.Flags, ctx.String("id"), ctx.String("condition"), ctx.String("target"), ctx.String("strategy"), ctx.Int("priority"), ctx.Bool("enabled"), ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "remove",
								Short:     "remove a routing rule",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "id"}},
								Flags: []FlagSpec{
									{Name: "yes", Kind: FlagBool, Usage: "confirm removal without prompting"},
								},
								Run: func(ctx *RunCtx) int {
									return runRoutingRulesRemove(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "toggle",
								Short:     "toggle a routing rule's enabled state",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "id"}},
								Run:       func(ctx *RunCtx) int { return runRoutingRulesToggle(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
							},
						},
					},
					{
						Name:      "strategy",
						Short:     "get/set the global routing strategy",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "get",
								Short:     "show the global routing strategy",
								NeedsAuth: true,
								Run:       func(ctx *RunCtx) int { return runRoutingStrategyGet(ctx.Flags, ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "set",
								Short:     "set the global routing strategy",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "strategy"}},
								Run:       func(ctx *RunCtx) int { return runRoutingStrategySet(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
							},
						},
					},
				},
			},
			{
				Name:      "cloud",
				Short:     "manage cloud overflow providers and view budget status",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "providers",
						Short:     "list/add/update/delete/reorder/test cloud providers",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "list",
								Short:     "list cloud providers (does not display the API key)",
								NeedsAuth: true,
								Run:       func(ctx *RunCtx) int { return runCloudProvidersList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
							},
							{
								Name:      "add",
								Short:     "add a cloud provider",
								NeedsAuth: true,
								Flags: []FlagSpec{
									{Name: "name", Kind: FlagString, Usage: "provider config name (required)", Required: true, RequiredMsg: "error: --name is required"},
									{Name: "provider", Kind: FlagString, Usage: "provider type, e.g. openai, anthropic, openrouter (required)", Required: true, RequiredMsg: "error: --provider is required"},
									{Name: "base-url", Kind: FlagString, DefString: "", Usage: "provider API base URL (required if --enabled)"},
									{Name: "api-key", Kind: FlagString, DefString: "", Usage: "provider API key (required if --enabled) - never echoed back by \"list\""},
									{Name: "default-model", Kind: FlagString, DefString: "", Usage: "default model for this provider"},
									{Name: "cost-per-1k", Kind: FlagString, DefString: "", Usage: "cost per 1K tokens in USD, for savings tracking"},
									{Name: "priority", Kind: FlagInt, DefInt: 0, Usage: "fallback priority (lower tries first)"},
									{Name: "enabled", Kind: FlagBool, Usage: "enable immediately"},
								},
								Run: func(ctx *RunCtx) int {
									return runCloudProvidersAdd(ctx.Flags, ctx.String("name"), ctx.String("provider"), ctx.String("base-url"), ctx.String("api-key"), ctx.String("default-model"), ctx.String("cost-per-1k"), ctx.Int("priority"), ctx.Bool("enabled"), ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "update",
								Short:     "update a cloud provider (omit --api-key to keep the stored key)",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "name"}},
								Flags: []FlagSpec{
									{Name: "provider", Kind: FlagString, DefString: "", Usage: "provider type, e.g. openai, anthropic, openrouter"},
									{Name: "base-url", Kind: FlagString, DefString: "", Usage: "provider API base URL"},
									{Name: "api-key", Kind: FlagString, DefString: "", Usage: "provider API key (omit to keep the currently stored key)"},
									{Name: "default-model", Kind: FlagString, DefString: "", Usage: "default model for this provider"},
									{Name: "cost-per-1k", Kind: FlagString, DefString: "", Usage: "cost per 1K tokens in USD, for savings tracking"},
									{Name: "priority", Kind: FlagInt, DefInt: 0, Usage: "fallback priority (lower tries first)"},
									{Name: "enabled", Kind: FlagBool, Usage: "enable this provider"},
								},
								Run: func(ctx *RunCtx) int {
									return runCloudProvidersUpdate(ctx, ctx.Args[0])
								},
							},
							{
								Name:      "delete",
								Short:     "delete a cloud provider",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "name"}},
								Flags: []FlagSpec{
									{Name: "yes", Kind: FlagBool, Usage: "confirm deletion without prompting"},
								},
								Run: func(ctx *RunCtx) int {
									return runCloudProvidersDelete(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "reorder",
								Short:     "set cloud provider fallback priority order",
								NeedsAuth: true,
								Args:      []ArgSpec{{Name: "names"}},
								Run: func(ctx *RunCtx) int {
									return runCloudProvidersReorder(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr)
								},
							},
							{
								Name:      "test",
								Short:     "verify a base-url+api-key pair authenticates, without saving it",
								NeedsAuth: true,
								Flags: []FlagSpec{
									{Name: "provider", Kind: FlagString, Usage: "provider type (required)", Required: true, RequiredMsg: "error: --provider is required"},
									{Name: "base-url", Kind: FlagString, Usage: "provider API base URL (required)", Required: true, RequiredMsg: "error: --base-url is required"},
									{Name: "api-key", Kind: FlagString, Usage: "provider API key to test (required)", Required: true, RequiredMsg: "error: --api-key is required"},
								},
								Run: func(ctx *RunCtx) int {
									return runCloudProvidersTest(ctx.Flags, ctx.String("provider"), ctx.String("base-url"), ctx.String("api-key"), ctx.Stdout, ctx.Stderr)
								},
							},
						},
					},
					{
						Name:      "budget-status",
						Short:     "show global and per-key cloud spend vs budget caps",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runCloudBudgetStatus(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "favorites",
				Short:     "manage your starred model list",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "list",
						Short:     "list starred model ids",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runFavoritesList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "add",
						Short:     "star a model",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "model-id"}},
						Run:       func(ctx *RunCtx) int { return runFavoritesAdd(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "remove",
						Short:     "unstar a model",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "model-id"}},
						Run:       func(ctx *RunCtx) int { return runFavoritesRemove(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:  "model-config",
				Short: "manage per-node model parameter profiles",
				Long: "store.ModelConfig has ~40 optional per-runtime sampling/load-time fields, so\n" +
					"\"set\" takes a JSON body via --from-json (a literal JSON string or\n" +
					"@path/to/file.json) rather than dozens of individual flags - see\n" +
					"internal/cli/modelconfig.go for the full field list and rationale.",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "get",
						Short:     "get a model's parameter profile on one node",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "model", Kind: FlagString, Usage: "model name (required)", Required: true, RequiredMsg: "error: --model is required"},
							{Name: "node", Kind: FlagString, Usage: "node name (required)", Required: true, RequiredMsg: "error: --node is required"},
						},
						Run: func(ctx *RunCtx) int {
							return runModelConfigGet(ctx.Flags, ctx.String("model"), ctx.String("node"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "set",
						Short:     "create/update a model's parameter profile (full JSON body)",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "from-json", Kind: FlagString, Usage: "JSON body (literal string, or @path/to/file.json) - must include \"model\" and \"node\" (required)", Required: true, RequiredMsg: "error: --from-json is required"},
						},
						Run: func(ctx *RunCtx) int {
							return runModelConfigSet(ctx.Flags, ctx.String("from-json"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "delete",
						Short:     "reset a model on a node to backend defaults",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "model", Kind: FlagString, Usage: "model name (required)", Required: true, RequiredMsg: "error: --model is required"},
							{Name: "node", Kind: FlagString, Usage: "node name (required)", Required: true, RequiredMsg: "error: --node is required"},
						},
						Run: func(ctx *RunCtx) int {
							return runModelConfigDelete(ctx.Flags, ctx.String("model"), ctx.String("node"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "list",
						Short:     "list every configured model parameter profile",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runModelConfigList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "capabilities",
						Short:     "show which parameter fields take effect per runtime",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runModelConfigCapabilities(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "catalog",
				Short:     "show the fleet-aware HF/local model catalog with per-node fit",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runCatalog(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:      "backup",
				Short:     "manage marbor.db backups",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "now",
						Short:     "trigger an on-demand backup and download it",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "output", Kind: FlagString, DefString: "", Usage: "local file path to save to (default: server-suggested filename)"},
						},
						Run: func(ctx *RunCtx) int { return runBackupNow(ctx.Flags, ctx.String("output"), ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "list",
						Short:     "list backup files on the server",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runBackupList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "restore",
						Short:     "restore marbor.db from a backup file (marbor restarts)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "filename"}},
						Flags: []FlagSpec{
							{Name: "yes", Kind: FlagBool, Usage: "confirm restore without prompting"},
						},
						Run: func(ctx *RunCtx) int {
							return runBackupRestore(ctx.Flags, ctx.Args[0], ctx.Bool("yes"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "upload",
						Short:     "upload a local .db file as a restorable backup",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "file", Kind: FlagString, Usage: "local .db file path (required)", Required: true, RequiredMsg: "error: --file is required"},
						},
						Run: func(ctx *RunCtx) int { return runBackupUpload(ctx.Flags, ctx.String("file"), ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "analytics",
				Short:     "hourly analytics + per-model stats",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "show",
						Short:     "show analytics (raw JSON)",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runAnalyticsShow(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "export",
						Short:     "export analytics to a local file",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "type", Kind: FlagString, DefString: "hourly", Usage: "hourly (default) or models"},
							{Name: "format", Kind: FlagString, DefString: "", Usage: "csv or json (default json)"},
							{Name: "output", Kind: FlagString, DefString: "", Usage: "local file path to save to (default: server-suggested filename)"},
						},
						Run: func(ctx *RunCtx) int {
							return runAnalyticsExport(ctx.Flags, ctx.String("type"), ctx.String("format"), ctx.String("output"), ctx.Stdout, ctx.Stderr)
						},
					},
				},
			},
			{
				Name:      "savings",
				Short:     "show cloud-vs-local savings summary",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runSavings(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:  "metrics",
				Short: "dashboard metrics",
				Sub: []*Command{
					{
						Name:      "summary",
						Short:     "show the dashboard summary strip (nodes, active requests, latency, tokens/min)",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runMetricsSummary(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "pulls",
				Short:     "list every active model pull job across the fleet",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runPulls(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:      "warmup",
				Short:     "global warmup engine status and manual controls",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "status",
						Short:     "show global warmup engine status",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runWarmupStatus(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "predictive",
						Short:     "enable/disable the predictive prewarm engine",
						NeedsAuth: true,
						Sub: []*Command{
							{
								Name:      "set",
								Short:     "enable/disable the predictive prewarm engine",
								NeedsAuth: true,
								Flags: []FlagSpec{
									{Name: "enabled", Kind: FlagBool, Usage: "enable the predictive engine"},
								},
								Run: func(ctx *RunCtx) int {
									return runWarmupPredictiveSet(ctx.Flags, ctx.Bool("enabled"), ctx.Stdout, ctx.Stderr)
								},
							},
						},
					},
					{
						Name:      "ping",
						Short:     "manually trigger a warmup cycle now",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runWarmupPing(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "predictive",
				Short:     "show recent predictive prewarm decisions",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "decisions",
						Short:     "show recent predictive prewarm decisions",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runPredictiveDecisions(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "system-info",
				Short:     "show control-plane host system info and per-node GPU summary",
				NeedsAuth: true,
				Footer:    authFlags,
				Run:       func(ctx *RunCtx) int { return runSystemInfo(ctx.Flags, ctx.Stdout, ctx.Stderr) },
			},
			{
				Name:  "config",
				Short: "control-plane configuration operations",
				Sub: []*Command{
					{
						Name:      "reload",
						Short:     "re-sync live router/auth state from SQLite",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runConfigReload(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "benchmark",
				Short:     "run/inspect in-dashboard hardware benchmark jobs",
				NeedsAuth: true,
				Footer:    authFlags,
				Sub: []*Command{
					{
						Name:      "run",
						Short:     "start a benchmark job",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "node"}, {Name: "model"}},
						Flags: []FlagSpec{
							{Name: "n", Kind: FlagInt, DefInt: 10, Usage: "number of cold/warm samples (1-50, default 10)"},
						},
						Run: func(ctx *RunCtx) int {
							return runBenchmarkRun(ctx.Flags, ctx.Args[0], ctx.Args[1], ctx.Int("n"), ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "progress",
						Short:     "show a point-in-time snapshot of a running benchmark job",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "job-id"}},
						Run:       func(ctx *RunCtx) int { return runBenchmarkProgress(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "cancel",
						Short:     "cancel an in-flight benchmark job",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "job-id"}},
						Run:       func(ctx *RunCtx) int { return runBenchmarkCancel(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "runs",
						Short:     "show persisted benchmark run history",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runBenchmarkRuns(ctx.Flags, ctx.Stdout, ctx.Stderr) },
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
				Long:      "Times are shown in UTC (RFC3339 Z) - the Admin API stores every audit event in UTC. The dashboard renders the same instants in the operator's configured timezone; this CLI shows the raw UTC value.",
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
					{
						Name:      "list",
						Short:     "show the in-memory request log, newest first",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runRequestsList(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "live",
						Short:     "show the same bounded request ring in its raw live-widget shape",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runRequestsLive(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
				},
			},
			{
				Name:      "audit",
				Short:     "inspect the persisted, filterable request audit log",
				Long:      "Distinct from \"activity\", which covers operator actions (drain/agent/runtime/node/warmup); \"audit\" covers individual proxied requests.",
				NeedsAuth: true,
				Footer:    authFlags,
				Flags: []FlagSpec{
					{Name: "limit", Kind: FlagInt, DefInt: 100, Usage: "max entries to show (1-1000, default 100)"},
					{Name: "model", Kind: FlagString, DefString: "", Usage: "filter by exact model name"},
					{Name: "key", Kind: FlagString, DefString: "", Usage: "filter by exact API key name"},
					{Name: "node", Kind: FlagString, DefString: "", Usage: "filter by exact node name"},
					{Name: "status", Kind: FlagString, DefString: "", Usage: "filter by status category: success, client_error, or server_error"},
					{Name: "cloud", Kind: FlagString, DefString: "", Usage: "filter by cloud fallback: true or false"},
					{Name: "since", Kind: FlagString, DefString: "", Usage: "filter from time (RFC3339)"},
					{Name: "until", Kind: FlagString, DefString: "", Usage: "filter to time (RFC3339)"},
				},
				Run: func(ctx *RunCtx) int {
					return runAudit(ctx.Flags, ctx.Int("limit"), ctx.String("model"), ctx.String("key"), ctx.String("node"), ctx.String("status"), ctx.String("cloud"), ctx.String("since"), ctx.String("until"), ctx.Stdout, ctx.Stderr)
				},
			},
			{
				Name:      "users",
				Short:     "manage dashboard users",
				NeedsAuth: true,
				Sub: []*Command{
					{
						Name:      "list",
						Short:     "list users",
						NeedsAuth: true,
						Run: func(ctx *RunCtx) int {
							return runUsersList(ctx.Flags, ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "create",
						Short:     "create a user (password printed once)",
						NeedsAuth: true,
						Flags: []FlagSpec{
							{Name: "user", Kind: FlagString, Usage: "username for the new user (required)", Required: true, RequiredMsg: "error: --user is required"},
							{Name: "email", Kind: FlagString, Usage: "email for the new user"},
							{Name: "role", Kind: FlagString, Usage: "role: admin or user"},
						},
						Run: func(ctx *RunCtx) int {
							return runUsersCreate(ctx.Flags, ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "approve",
						Short:     "approve a pending user",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Flags: []FlagSpec{
							{Name: "api-key-name", Kind: FlagString, Usage: "API key name to assign"},
							{Name: "create-key", Kind: FlagBool, Usage: "create an API key for the user"},
							{Name: "key-rate-limit", Kind: FlagInt, Usage: "rate limit for the new key (per hour)"},
							{Name: "key-daily-limit", Kind: FlagInt, Usage: "daily limit for the new key"},
							{Name: "key-monthly-limit", Kind: FlagInt, Usage: "monthly limit for the new key"},
							{Name: "key-models", Kind: FlagString, Usage: "comma-separated allowed models for the new key"},
						},
						Run: func(ctx *RunCtx) int {
							return runUsersApprove(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "suspend",
						Short:     "suspend a user and revoke sessions",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Flags: []FlagSpec{
							{Name: "yes", Kind: FlagBool, Usage: "confirm suspension without prompting"},
						},
						Run: func(ctx *RunCtx) int {
							return runUsersSuspend(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "reset-password",
						Short:     "reset a user's password (printed once)",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Run: func(ctx *RunCtx) int {
							return runUsersResetPassword(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr)
						},
					},
					{
						Name:      "patch",
						Short:     "update a user's email or role",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Flags: []FlagSpec{
							{Name: "email", Kind: FlagString, Usage: "new email"},
							{Name: "role", Kind: FlagString, Usage: "new role: admin or user"},
						},
						Run: func(ctx *RunCtx) int {
							return runUsersPatch(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "delete",
						Short:     "delete a user",
						NeedsAuth: true,
						Args:      []ArgSpec{{Name: "id"}},
						Flags: []FlagSpec{
							{Name: "yes", Kind: FlagBool, Usage: "confirm deletion without prompting"},
						},
						Run: func(ctx *RunCtx) int {
							return runUsersDelete(ctx.Flags, ctx.Args[0], ctx.Stdout, ctx.Stderr, ctx)
						},
					},
					{
						Name:      "pending-count",
						Short:     "show the number of users awaiting approval",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runUsersPendingCount(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "change-password",
						Short:     "change your own password (interactive, masked prompts)",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runUsersChangePassword(ctx.Flags, ctx.Stdout, ctx.Stderr) },
					},
					{
						Name:      "skip-password-change",
						Short:     "dismiss the forced-password-change prompt for this session only",
						NeedsAuth: true,
						Run:       func(ctx *RunCtx) int { return runUsersSkipPasswordChange(ctx.Flags, ctx.Stdout, ctx.Stderr) },
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
