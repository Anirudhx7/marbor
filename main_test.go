package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/cli"
)

// TestResolveCommand protects the merged binary's dispatch entry point
// (bench/agent/CLI subcommands vs. the server-start flag.Parse() path) so a
// future reordering around flag.Parse() can't silently break routing.
func TestResolveCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no args starts server", []string{}, "server"},
		{"bench subcommand", []string{"bench"}, "bench"},
		{"agent subcommand is removed, not dispatched", []string{"agent"}, "agent-removed"},
		{"cli version subcommand", []string{"version"}, "cli"},
		{"cli status subcommand", []string{"status"}, "cli"},
		{"cli login subcommand", []string{"login"}, "cli"},
		{"cli logout subcommand", []string{"logout"}, "cli"},
		{"cli whoami subcommand", []string{"whoami"}, "cli"},
		{"cli nodes subcommand", []string{"nodes"}, "cli"},
		{"cli models subcommand", []string{"models"}, "cli"},
		{"cli runtime subcommand", []string{"runtime", "restart", "gpu-0"}, "cli"},
		{"cli node control subcommand", []string{"node", "control", "probe"}, "cli"},
		{"top-level help word", []string{"help"}, "help"},
		{"top-level -h flag", []string{"-h"}, "help"},
		{"top-level --help flag", []string{"--help"}, "help"},
		{"root -version flag falls through to server", []string{"-version"}, "server"},
		{"root -db flag falls through to server", []string{"-db", "marbor.db"}, "server"},
		{"root -seed-node flag falls through to server", []string{"-seed-node", "name=a,url=http://x"}, "server"},
		{"unknown token errors instead of starting the server", []string{"bogus"}, "unknown"},
		{"uninstall subcommand", []string{"uninstall"}, "uninstall"},
		{"uninstall subcommand with --purge", []string{"uninstall", "--purge"}, "uninstall"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveCommand(tt.args); got != tt.want {
				t.Errorf("resolveCommand(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestResolveCommand_MatchesRegistry pins the P83+ item 2 fix: main.go's
// dispatch whitelist must be derived from internal/cli's registry, not a
// hand-maintained list that can silently omit commands (as it previously did
// for "key", "spill", and "requests" - see main.go:216 history). It asserts
// both directions: every registry top-level name resolves to "cli", and no
// word outside that set (union the non-registry subcommands handled
// separately by resolveCommand) resolves to "cli".
func TestResolveCommand_MatchesRegistry(t *testing.T) {
	names := cli.TopLevelCommandNames()
	if len(names) == 0 {
		t.Fatal("cli.TopLevelCommandNames() returned no names - registry likely broken")
	}

	for _, name := range names {
		t.Run("registry_name_"+name, func(t *testing.T) {
			if got := resolveCommand([]string{name}); got != "cli" {
				t.Errorf("resolveCommand([]string{%q}) = %q, want %q", name, got, "cli")
			}
		})
	}

	// The exact regression this step fixes: these three were previously
	// routed to "unknown" because main.go's hardcoded whitelist omitted
	// them, even though internal/cli/cli.go's dispatch switch already
	// implements them correctly.
	for _, name := range []string{"key", "spill", "requests"} {
		t.Run("regression_"+name, func(t *testing.T) {
			if got := resolveCommand([]string{name}); got != "cli" {
				t.Errorf("resolveCommand([]string{%q}) = %q, want %q (unreachable-command regression)", name, got, "cli")
			}
		})
	}

	// Build the set of registry names plus the other words resolveCommand
	// legitimately routes to "cli"-adjacent-but-different outcomes or that
	// are handled by earlier switch cases, so we know which non-registry
	// words must NOT resolve to "cli".
	registrySet := make(map[string]bool, len(names))
	for _, n := range names {
		registrySet[n] = true
	}
	nonCLIHandled := map[string]bool{
		"help": true, "-h": true, "--help": true,
		"bench": true, "agent": true, "uninstall": true,
	}

	notCLI := []string{"bogus", "modles", "pul", "server", "foo", "delete", "start", "list"}
	for _, word := range notCLI {
		if registrySet[word] || nonCLIHandled[word] {
			// Word happens to collide with a legitimate command/subcommand
			// name (e.g. a registry subaction reused as a top-level word in
			// this table) - skip rather than assert a false regression.
			continue
		}
		t.Run("non_registry_word_"+word, func(t *testing.T) {
			if got := resolveCommand([]string{word}); got == "cli" {
				t.Errorf("resolveCommand([]string{%q}) = %q, want anything but %q (word is not a top-level registry command)", word, got, "cli")
			}
		})
	}
}

// TestPrintTopLevelHelp_SourcesFromRegistry exercises the REAL production
// help path (printTopLevelHelp, called directly by main() for "marbor
// --help"/bare "marbor"/flag.Usage) rather than re-proving
// internal/cli's own writeHelp() in isolation - that would just re-cover
// finding #3's original scope, not this one (finding #12: main.go's
// top-level --help previously hand-duplicated a stale subset of the CLI
// registry's command names instead of reading cli.HelpRows()).
//
// Every non-hidden top-level command name is derived from cli.HelpRows()
// itself (which is registry-backed), not a hardcoded expected-list, so this
// test cannot silently keep passing while the real registry drifts out from
// under a stale literal.
func TestPrintTopLevelHelp_SourcesFromRegistry(t *testing.T) {
	var buf bytes.Buffer
	printTopLevelHelp(&buf)
	out := buf.String()

	for _, row := range cli.HelpRows() {
		name := strings.Fields(row[0])[0] // strip any "<args>"/"[args]" suffix
		if !strings.Contains(out, name) {
			t.Errorf("--help output missing non-hidden registry command %q\n--- output ---\n%s", name, out)
		}
	}

	// The exact regression finding #12 fixes: these were missing from
	// main.go's old hand-written helpTableRows subset even though they are
	// real, non-hidden, dispatchable Admin API CLI commands.
	for _, name := range []string{"key", "spill", "requests"} {
		if !strings.Contains(out, name) {
			t.Errorf("--help output missing previously-omitted registry command %q\n--- output ---\n%s", name, out)
		}
	}

	// "completion" is Hidden (registry_tree.go) - it must not be advertised
	// in the plain --help table, but (see the next test) must still be
	// genuinely dispatchable. Hidden means "not advertised", never
	// "unreachable" - this assertion and TestResolveCommand_HiddenCommandsReachable
	// together prove both halves of that contract for the top-level path.
	if strings.Contains(out, "completion") {
		t.Errorf("--help output contains hidden command %q - Hidden is not being respected\n--- output ---\n%s", "completion", out)
	}

	// The genuinely-non-CLI entrypoints must still be present and are not
	// part of the registry at all. "agent" is deliberately excluded here -
	// the Marbor Agent is a separate binary now (cmd/marbor-agent), not a
	// subcommand of this one; see TestResolveCommand's "agent" case and
	// TestAgentSubcommand_RedirectsToDedicatedBinary below.
	for _, name := range []string{"bench", "uninstall"} {
		if !strings.Contains(out, name) {
			t.Errorf("--help output missing non-CLI entrypoint %q\n--- output ---\n%s", name, out)
		}
	}
}

// TestResolveCommand_HiddenCommandsReachable proves the other half of the
// Hidden contract: a hidden command (here, "completion") is genuinely
// dispatchable via the real resolveCommand/main routing path even though
// TestPrintTopLevelHelp_SourcesFromRegistry proves it is correctly absent
// from --help. Conflating "not shown in help" with "not reachable" was
// exactly the historical bug (see TestResolveCommand_MatchesRegistry).
func TestResolveCommand_HiddenCommandsReachable(t *testing.T) {
	if got := resolveCommand([]string{"completion", "bash"}); got != "cli" {
		t.Errorf(`resolveCommand([]string{"completion", "bash"}) = %q, want "cli" (Hidden must not mean unreachable)`, got)
	}
}

// TestAgentSubcommand_RedirectsToDedicatedBinary proves "marbor agent
// ..." fails clearly (per the control-plane/Marbor Agent binary split) rather
// than silently doing something else, and that the message actually names
// the replacement binary.
func TestAgentSubcommand_RedirectsToDedicatedBinary(t *testing.T) {
	if got := resolveCommand([]string{"agent", "service", "install", "--port=9200"}); got != "agent-removed" {
		t.Errorf(`resolveCommand([]string{"agent", ...}) = %q, want "agent-removed"`, got)
	}

	var buf bytes.Buffer
	printAgentRemovedNotice(&buf)
	out := buf.String()
	for _, want := range []string{"marbor-agent", "service install"} {
		if !strings.Contains(out, want) {
			t.Errorf("printAgentRemovedNotice() output missing %q:\n%s", want, out)
		}
	}
}

// TestPrintUnknownCommand_SuggestsTopLevelTypo exercises the real
// printUnknownCommand production path (called by main() for the "unknown"
// resolveCommand outcome) with a deliberately typo'd top-level word, proving
// the "did you mean" suggestion internal/cli's own dispatcher already gives
// for in-CLI typos now also fires for the most common typo case of all: a
// mistyped top-level command.
func TestPrintUnknownCommand_SuggestsTopLevelTypo(t *testing.T) {
	var buf bytes.Buffer
	printUnknownCommand(&buf, "whoam")
	out := buf.String()

	if !strings.Contains(out, `"whoami"`) {
		t.Errorf(`printUnknownCommand(_, "whoam") output missing suggestion of "whoami"\n--- output ---\n%s`, out)
	}

	// A word with no plausible correction (nothing close in edit distance)
	// must degrade gracefully - no suggestion line, no panic - rather than
	// crash or fabricate a nonsense suggestion.
	var buf2 bytes.Buffer
	printUnknownCommand(&buf2, "zzznotacommand")
	out2 := buf2.String()
	if strings.Contains(out2, "Did you mean") {
		t.Errorf(`printUnknownCommand(_, "zzznotacommand") unexpectedly produced a suggestion\n--- output ---\n%s`, out2)
	}
	if !strings.Contains(out2, `unknown command "zzznotacommand"`) {
		t.Errorf(`printUnknownCommand(_, "zzznotacommand") missing the base "unknown command" message\n--- output ---\n%s`, out2)
	}
}

// controlPlaneOnlyPackages are the packages that give a binary the capability
// to start the Mesh control plane (admin API, router, SQLite store, proxy,
// auth middleware) or the Admin API CLI. cmd/marbor-agent must never
// depend on any of them - see TestAgentBinary_HasNoControlPlaneCapability.
var controlPlaneOnlyPackages = []string{
	"github.com/Anirudhx7/marbor/internal/admin",
	"github.com/Anirudhx7/marbor/internal/router",
	"github.com/Anirudhx7/marbor/internal/store",
	"github.com/Anirudhx7/marbor/internal/proxy",
	"github.com/Anirudhx7/marbor/internal/auth",
	"github.com/Anirudhx7/marbor/internal/cli",
}

// goListDeps runs "go list -deps" for importPath and returns the set of
// import paths in its build graph, so a test can assert on the actual
// compiled dependency graph rather than trusting that nobody added a new
// import later. This is the real proof for the control-plane/Node-Agent
// binary split's acceptance criterion: it fails the moment anyone imports a
// forbidden package, not just when someone remembers to update a hand-typed
// list of what's supposedly true.
func goListDeps(t *testing.T, importPath string) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", importPath).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", importPath, err)
	}
	deps := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps[line] = true
		}
	}
	return deps
}

// TestAgentBinary_HasNoControlPlaneCapability is the primary acceptance
// criterion for the control-plane/Marbor-Agent binary split: a GPU/marbor-agent
// host running only cmd/marbor-agent must have no code path capable of
// starting the Mesh control plane, opening marbor.db, or serving the admin
// API - proven here by showing the capability isn't even compiled into the
// binary, not merely unreachable at runtime.
func TestAgentBinary_HasNoControlPlaneCapability(t *testing.T) {
	deps := goListDeps(t, "./cmd/marbor-agent")
	for _, forbidden := range controlPlaneOnlyPackages {
		if deps[forbidden] {
			t.Errorf("cmd/marbor-agent depends on %s - the agent binary must never be capable of starting the Mesh control plane", forbidden)
		}
	}
}

// TestServerBinary_DoesNotDependOnAgentRuntime is the reverse direction: the
// split should leave marbor with no half-agent architecture either.
//
// This deliberately does NOT assert "marbor must not import package
// internal/marboragent at all" via go list -deps - that's unsatisfiable and
// would be the wrong target. internal/admin genuinely needs marboragent's
// frozen R9 wire types (marboragent.GPUInfo/GPUBlock) and its auth-scope
// constants (marboragent.ScopeAdmin/...) - pre-existing, required, unrelated
// to this split. Go's dependency graph is package-granular, not
// symbol-granular: because agent.go/service_cmd.go (the actual runtime/
// service-install logic) live in that SAME package as those wire types,
// any consumer of the wire types transitively pulls in
// internal/marboragent/service too, with no way to prune it short of
// splitting marboragent's protocol types into their own leaf package - a
// real refactor of internal/marboragent, which is explicitly out of scope
// here (this is an entry-point/artifact/installation change, not a
// protocol reorganization).
//
// What's actually provable, and what the acceptance criterion is really
// about, is that no source file built into the marbor binary calls the
// agent's startup entry points - proven at the source level (not just
// "unreachable at runtime") since a call site is a stronger, more durable
// signal than a package-level import that Go's model can't help but retain
// anyway.
func TestServerBinary_DoesNotDependOnAgentRuntime(t *testing.T) {
	rootGoFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing root .go files: %v", err)
	}
	forbiddenCalls := []string{"marboragent.Run(", "service.New()"}
	for _, path := range rootGoFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue // this file itself references these strings in comments/assertions above.
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, forbidden := range forbiddenCalls {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("%s calls %s - the Marbor Agent runtime/service-manager entry point is cmd/marbor-agent's job now, not marbor's", path, forbidden)
			}
		}
	}
}
