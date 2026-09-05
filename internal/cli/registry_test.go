package cli

import (
	"regexp"
	"testing"
)

// TestRegistry_TreeValid pins the CLI command registry's structural
// invariants: finalize must accept the real tree, must not run until
// something asks for it, and must reject each of the malformed shapes it
// claims to catch.
func TestRegistry_TreeValid(t *testing.T) {
	// This test no longer asserts rootBuilt == false on entry: since
	// help.go's writeHelp wired the six print*Usage functions to call
	// root() (P83+ CLI hardening plan, migration step 4), any other test in
	// this package that exercises a "--help"/usage path for models,
	// runtime, login, node control, key, or requests may legitimately
	// trigger buildRoot before this test runs - that is normal use, not an
	// init()-time eager build. The invariant this guarded against (no
	// package-level init() triggers buildRoot) is a static fact checkable
	// by inspection - this package declares no init() function - rather
	// than something provable by test ordering once root() has real,
	// non-test callers.
	r1 := root()
	if !rootBuilt {
		t.Fatal("root() did not trigger buildRoot")
	}
	r2 := root()
	if r1 != r2 {
		t.Fatal("root() should memoize via sync.OnceValue and return the same *Command on every call")
	}
	if r1.Name != "marbor" {
		t.Fatalf("root name = %q, want %q", r1.Name, "marbor")
	}

	// Spot-check a few known leaves/groups exist with the expected shape,
	// so a future edit that silently drops or renames a command fails here
	// rather than only in a later migration step.
	wantTop := []string{"version", "status", "login", "logout", "whoami", "nodes", "models", "runtime", "node", "key", "spill", "requests"}
	for _, name := range wantTop {
		if r1.lookup(name) == nil {
			t.Errorf("root is missing top-level command %q", name)
		}
	}

	models := r1.lookup("models")
	if models == nil {
		t.Fatal("models command missing")
	}
	for _, action := range []string{"pull", "delete", "unload", "list"} {
		if models.lookup(action) == nil {
			t.Errorf("models is missing action %q", action)
		}
	}
	if pull := models.lookup("pull"); pull != nil {
		if pull.MinArgs() != 2 || pull.MaxArgs() != 2 {
			t.Errorf("models pull MinArgs/MaxArgs = %d/%d, want 2/2", pull.MinArgs(), pull.MaxArgs())
		}
		if got, want := pull.Path(), "marbor models pull"; got != want {
			t.Errorf("models pull Path() = %q, want %q", got, want)
		}
	}

	node := r1.lookup("node")
	if node == nil || node.lookup("control") == nil {
		t.Fatal("node control missing")
	}
	control := node.lookup("control")
	if control.lookup("probe") == nil || control.lookup("accept") == nil {
		t.Error("node control is missing probe or accept")
	}
}

// TestRegistry_Finalize_Panics builds small malformed trees inline and
// asserts finalize panics on each - the specific invariants
// TestRegistry_TreeValid's happy path can't otherwise prove are enforced.
func TestRegistry_Finalize_Panics(t *testing.T) {
	cases := map[string]func() *Command{
		"duplicate sibling name": func() *Command {
			return &Command{
				Name: "root",
				Sub: []*Command{
					{Name: "dup", Run: notYetMigrated},
					{Name: "dup", Run: notYetMigrated},
				},
			}
		},
		"duplicate alias colliding with a sibling name": func() *Command {
			return &Command{
				Name: "root",
				Sub: []*Command{
					{Name: "a", Run: notYetMigrated},
					{Name: "b", Aliases: []string{"a"}, Run: notYetMigrated},
				},
			}
		},
		"duplicate alias colliding with another alias": func() *Command {
			return &Command{
				Name: "root",
				Sub: []*Command{
					{Name: "a", Aliases: []string{"x"}, Run: notYetMigrated},
					{Name: "b", Aliases: []string{"x"}, Run: notYetMigrated},
				},
			}
		},
		"required arg after optional arg": func() *Command {
			return &Command{
				Name: "root",
				Args: []ArgSpec{
					{Name: "first", Optional: true},
					{Name: "second"},
				},
				Run: notYetMigrated,
			}
		},
		"two variadic args": func() *Command {
			return &Command{
				Name: "root",
				Args: []ArgSpec{
					{Name: "first", Variadic: true},
					{Name: "second", Variadic: true},
				},
				Run: notYetMigrated,
			}
		},
		"variadic not last": func() *Command {
			return &Command{
				Name: "root",
				Args: []ArgSpec{
					{Name: "first", Variadic: true},
					{Name: "second"},
				},
				Run: notYetMigrated,
			}
		},
		"flag with empty usage": func() *Command {
			return &Command{
				Name:  "root",
				Flags: []FlagSpec{{Name: "quiet", Kind: FlagBool, Usage: ""}},
				Run:   notYetMigrated,
			}
		},
		"leaf with no Run and no Sub": func() *Command {
			return &Command{Name: "root"}
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("finalize did not panic for case %q", name)
				}
			}()
			finalize(build())
		})
	}
}

// TestRegistry_Finalize_AllowsNilRunWithSub confirms the one shape that must
// NOT panic: a node with Run == nil is fine as long as it has at least one
// Sub (a command group like "models" or "node control").
func TestRegistry_Finalize_AllowsNilRunWithSub(t *testing.T) {
	c := &Command{
		Name: "root",
		Sub: []*Command{
			{Name: "child", Run: notYetMigrated},
		},
	}
	finalize(c) // must not panic
}

// internalIDPattern catches internal-only references that must never reach
// user-facing text (--help output, generated docs, man pages): queue/ticket
// IDs (P411, P-A2-09b), guard citations (R8, R10), Law references (Law 6),
// and LESSONS refs (L23). It is intentionally case-sensitive - lowercase
// "p50"/"p95"/"p99" (latency percentiles) are legitimate product vocabulary,
// not ticket IDs, and must not be flagged (P415).
var internalIDPattern = regexp.MustCompile(
	`\bP-?[A-Z0-9]*-?\d{2,}[a-z]?\b` + // P411, P84, P-A2-09b
		`|\bR\d{1,2}\b` + // R8, R10 guard shorthand
		`|\bLaw\s*\d+\b` + // Law 6
		`|\bL\d{2,}\b`, // L23 LESSONS ref
)

// internalIDExceptions lists exact matched substrings that look like ticket
// IDs to the regex above but are real, legitimate product terms. Add an
// entry here (with a one-line reason) rather than weakening the pattern -
// the pattern is the guard; this list is the documented, reviewable escape
// hatch for the rare true false-positive (e.g. a GPU model number). Entries
// with no occurrence in any help string are removed outright (dormant
// entries can only hide a future genuine cite) - re-add with a reason if a
// real use ever lands. Currently empty: no help string needs an exception.
var internalIDExceptions = map[string]string{}

// checkInternalLeak fails t if s contains an internal-only reference not
// covered by internalIDExceptions. path/field identify where s came from so
// a failure points straight at the string to fix.
func checkInternalLeak(t *testing.T, path, field, s string) {
	t.Helper()
	for _, m := range internalIDPattern.FindAllString(s, -1) {
		if _, ok := internalIDExceptions[m]; ok {
			continue
		}
		t.Errorf("%s: %s contains internal-only reference %q in %q - reword for an operator who has never seen this codebase's internal docs, or add a documented exception to internalIDExceptions if this is a genuine false positive", path, field, m, s)
	}
}

// walkForLeaks recursively checks every Short/Long/Footer/flag-usage string
// reachable from c against internalIDPattern.
func walkForLeaks(t *testing.T, c *Command, path string) {
	t.Helper()
	p := path
	if c.Name != "" {
		if p == "" {
			p = c.Name
		} else {
			p = p + " " + c.Name
		}
	}
	checkInternalLeak(t, p, "Short", c.Short)
	checkInternalLeak(t, p, "Long", c.Long)
	checkInternalLeak(t, p, "Footer", c.Footer)
	for _, f := range c.Flags {
		checkInternalLeak(t, p, "Flag[--"+f.Name+"].Usage", f.Usage)
	}
	for _, sub := range c.Sub {
		walkForLeaks(t, sub, p)
	}
}

// TestRegistry_NoInternalIDLeakage is the standing guard for P415: it walks
// the full command tree and fails if any user-facing help string still
// carries an internal queue-item ID, guard/Law reference, or LESSONS ref.
// Runs under the existing `go test ./...` step in scripts/gate.sh - no
// separate CI wiring needed. If this test is failing, the fix is rewording
// the flagged string in registry_tree.go in plain language, not touching
// this test.
func TestRegistry_NoInternalIDLeakage(t *testing.T) {
	walkForLeaks(t, root(), "")
}
