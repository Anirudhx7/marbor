package main

import "github.com/ollama-mesh/ollama-mesh/internal/cli"

// registry_walk.go holds helpers shared by man.go/markdown.go/readme.go for
// reading internal/cli's Command tree. cli.Command's fields (Name, Aliases,
// Short, Long, Footer, Args, Flags, Sub, Hidden, NeedsAuth, Examples) are all
// exported, so this package can walk the tree directly; only a handful of
// small rendering helpers (argsSuffix, flag signature rendering) are
// re-implemented here because their internal/cli equivalents (help.go) are
// unexported.

// globalFlag mirrors one row of internal/cli's unexported authFlagsRows
// (cli.go) - re-derived here rather than imported (it isn't exported, and
// the plan explicitly allows hardcoding the 4 known global flags since the
// registry doesn't capture them as a first-class per-command concept; every
// leaf accepts them unconditionally via newFlagSet).
type globalFlag struct {
	Name  string
	Usage string
}

// globalFlags is declaration order, matching cli.go's authFlagsRows text
// verbatim so generated docs never say something different from --help.
var globalFlags = []globalFlag{
	{"server", `Admin API base URL (default "http://localhost:8080", env MARBOR_SERVER)`},
	{"json", "output machine-readable JSON instead of a human table"},
	{"username", "admin username, used to log in (env MARBOR_USERNAME)"},
	{"password", "admin password, used to log in (env MARBOR_PASSWORD)"},
}

// groupPageCommands returns, in declared order, every top-level command that
// gets its own generated man/doc page - i.e. has at least one subcommand
// ("nodes" qualifies: it has both a Run and a "confirm-tls" Sub). Leaf
// top-level commands (version, status, login, logout, whoami, spill) are
// documented inline in the root page only; "completion" is Hidden and has no
// subcommands, so it also has no group page - the root page mentions it
// briefly instead.
func groupPageCommands(root *cli.Command) []*cli.Command {
	var groups []*cli.Command
	for _, c := range root.Sub {
		if len(c.Sub) > 0 {
			groups = append(groups, c)
		}
	}
	return groups
}

// pageSlug returns the man/doc page identifier for a top-level group
// command, e.g. "marbor-models".
func pageSlug(root *cli.Command, c *cli.Command) string {
	return root.Name + "-" + c.Name
}

// argsSuffix renders args using the same "<required>"/"[optional]"/
// "name..." convention as internal/cli's argsSuffix (help.go) and
// Command.UsageLine - reimplemented here since that helper is unexported.
func argsSuffix(args []cli.ArgSpec) string {
	s := ""
	for _, a := range args {
		name := a.Name
		if a.Variadic {
			name += "..."
		}
		if a.Optional {
			s += " [" + name + "]"
		} else {
			s += " <" + name + ">"
		}
	}
	return s
}

// flagSignature renders one flag's "--name" (plus " string"/" int" for
// non-bool kinds), matching internal/cli's help.go flagRows.
func flagSignature(f cli.FlagSpec) string {
	name := "--" + f.Name
	switch f.Kind {
	case cli.FlagString:
		name += " string"
	case cli.FlagInt:
		name += " int"
	}
	return name
}

// visibleFlags returns c's own non-hidden flags, declaration order.
func visibleFlags(c *cli.Command) []cli.FlagSpec {
	var out []cli.FlagSpec
	for _, f := range c.Flags {
		if !f.Hidden {
			out = append(out, f)
		}
	}
	return out
}

// flattenDescendants returns every descendant of c (not c itself), via a
// declaration-order depth-first walk - used by group man/markdown pages to
// document arbitrarily nested commands (e.g. "node control probe/accept")
// as one flat list.
func flattenDescendants(c *cli.Command) []*cli.Command {
	var out []*cli.Command
	var walk func(n *cli.Command)
	walk = func(n *cli.Command) {
		for _, s := range n.Sub {
			out = append(out, s)
			walk(s)
		}
	}
	walk(c)
	return out
}

// allExamples collects every non-empty Examples entry found anywhere in the
// tree rooted at c (including c itself), in declaration order, deduplicated.
// The dedup map is only ever used for a membership check, never iterated -
// output order comes entirely from the walk, so this stays deterministic.
func allExamples(c *cli.Command) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(n *cli.Command)
	walk = func(n *cli.Command) {
		for _, ex := range n.Examples {
			if !seen[ex] {
				seen[ex] = true
				out = append(out, ex)
			}
		}
		for _, s := range n.Sub {
			walk(s)
		}
	}
	walk(c)
	return out
}
