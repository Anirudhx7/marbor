package cli

import "testing"

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
	if r1.Name != "ollama-mesh" {
		t.Fatalf("root name = %q, want %q", r1.Name, "ollama-mesh")
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
		if got, want := pull.Path(), "ollama-mesh models pull"; got != want {
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
