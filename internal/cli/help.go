package cli

import (
	"fmt"
	"io"
	"strings"
)

// help.go generates CLI help/usage text from the command registry
// (registry.go, registry_tree.go), instead of the hand-written print*Usage
// functions and hand-aligned `usage` const in cli.go, part of the CLI
// hardening plan's registry migration -
// alignment goes through the same renderTable/tabwriter helper the six
// print*Usage functions already used, so it can no longer drift the way the
// hand-spaced `usage` const in cli.go has.
//
// As of this step, writeHelp is wired ONLY behind the six existing
// print*Usage functions (now thin wrappers - see the bottom of cli.go,
// keys.go, requests.go). The top-level `usage` const and the six commands
// that pass usage=nil into parseFlags are migrated in later plan steps.

// findCommand walks path under a starting node (typically root()), matching
// each segment against Command.lookup (name, then aliases). Returns nil if
// any segment doesn't match. Used by the print*Usage wrapper shims below to
// locate the registry node whose help they should render.
func findCommand(from *Command, path ...string) *Command {
	cur := from
	for _, p := range path {
		if cur == nil {
			return nil
		}
		cur = cur.lookup(p)
	}
	return cur
}

// writeHelp renders help for c in one of three shapes, depending on where c
// sits in the tree:
//   - root (c.parent == nil, i.e. c is root() itself): full command list.
//   - group (has Sub): action table for this group's children. Classified
//     on len(c.Sub), not c.Run == nil - "models" has both a Run (the
//     fleet-wide list shown when invoked with no action) and a Sub list
//     (pull/delete/unload/list), and must still render as a group so its
//     actions table appears; Run alone does not mean "leaf".
//   - leaf (no Sub): synopsis, description, flags, examples.
func writeHelp(w io.Writer, c *Command) {
	if c == nil {
		return
	}
	switch {
	case c.parent == nil:
		writeRootHelp(w, c)
	case len(c.Sub) > 0:
		writeGroupHelp(w, c)
	default:
		writeLeafHelp(w, c)
	}
}

// argsSuffix renders c's positional args using the same "<required>"/
// "[optional]"/"name..." convention as Command.UsageLine, as a standalone
// fragment (no leading path, no trailing "[flags]") so it can be reused both
// in a command-table row's name column and in a leaf's own usage line.
func argsSuffix(args []ArgSpec) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString(" ")
		name := a.Name
		if a.Variadic {
			name += "..."
		}
		if a.Optional {
			b.WriteString("[" + name + "]")
		} else {
			b.WriteString("<" + name + ">")
		}
	}
	return b.String()
}

// childRows renders a command table (name column includes positional args,
// short description) over c's non-hidden direct children.
func childRows(subs []*Command) [][2]string {
	rows := make([][2]string, 0, len(subs))
	for _, s := range subs {
		if s.Hidden {
			continue
		}
		rows = append(rows, [2]string{s.Name + argsSuffix(s.Args), s.Short})
	}
	return rows
}

// flagRows renders c's own declared flags (not the global auth flags - see
// flagsWithAuth/leafFlagRows) as a command table.
func flagRows(flags []FlagSpec) [][2]string {
	rows := make([][2]string, 0, len(flags))
	for _, f := range flags {
		if f.Hidden {
			continue
		}
		name := "--" + f.Name
		switch f.Kind {
		case FlagString:
			name += " string"
		case FlagInt:
			name += " int"
		}
		rows = append(rows, [2]string{name, f.Usage})
	}
	return rows
}

// flagsWithAuth returns c's own flags plus, if c.NeedsAuth, the global auth
// flags - as one combined table so a single renderTable/tabwriter call
// aligns both consistently (splitting into two renderTable calls would let
// each compute independent column widths and misalign against each other).
func flagsWithAuth(c *Command) [][2]string {
	rows := flagRows(c.Flags)
	if c.NeedsAuth {
		rows = append(rows, authFlagsRows...)
	}
	return rows
}

// leafFlagRows returns c's own flags plus the global auth flags,
// unconditionally: every leaf command accepts --server/--json/--username/
// --password via newFlagSet regardless of whether it declares NeedsAuth (a
// leaf's NeedsAuth reflects whether authenticatedClient is actually called,
// not whether the flags are accepted), so leaf help always documents them.
func leafFlagRows(c *Command) [][2]string {
	return append(flagRows(c.Flags), authFlagsRows...)
}

// HelpRows returns (name[+args], short-description) pairs for every
// non-hidden top-level CLI command, in registry declaration order - the
// exact same rows writeRootHelp's own command table renders (via
// childRows(c.Sub) below), exposed so main.go's top-level --help/bare-command
// output can fold the real Admin API CLI command list into its own combined
// table instead of hand-maintaining a second, independently-drifting copy of
// this same data (see the CLI hardening review's findings: main.go
// previously hand-listed a stale subset of these names). Hidden commands
// (e.g. "completion") are intentionally omitted here, matching childRows'
// own Hidden check - main.go should not advertise them either. Reusing
// childRows rather than writing a second row-builder is deliberate: two
// independent implementations computing "the same" rows is exactly the kind
// of duplication finding #3's fix eliminated once already.
func HelpRows() [][2]string {
	return childRows(root().Sub)
}

func writeRootHelp(w io.Writer, c *Command) {
	fmt.Fprintf(w, "%s - %s\n\n", c.Name, c.Short)
	fmt.Fprintf(w, "Usage:\n  %s <command> [flags]\n\nCommands:\n", c.Name)
	renderTable(w, "  ", childRows(c.Sub))
	fmt.Fprintf(w, "\nRun \"%s <command> --help\" for the full list of actions and flags for\nthat command.\n\n", c.Name)
	fmt.Fprint(w, "Global flags:\n")
	renderTable(w, "  ", authFlagsRows)
	if c.Footer != "" {
		fmt.Fprintf(w, "\n%s\n", c.Footer)
	}
}

func writeGroupHelp(w io.Writer, c *Command) {
	fmt.Fprintf(w, "Usage: %s [action] [args] [flags]\n\nActions:\n", c.Path())
	renderTable(w, "  ", childRows(c.Sub))
	fmt.Fprint(w, "\nFlags:\n")
	renderTable(w, "  ", flagsWithAuth(c))
	if c.Long != "" {
		fmt.Fprintf(w, "\n%s\n", c.Long)
	}
	if c.Footer != "" {
		fmt.Fprintf(w, "\n%s\n", c.Footer)
	}
}

func writeLeafHelp(w io.Writer, c *Command) {
	fmt.Fprintf(w, "Usage: %s%s [flags]\n\n", c.Path(), argsSuffix(c.Args))
	if c.Long != "" {
		fmt.Fprintf(w, "%s\n\n", c.Long)
	}
	fmt.Fprint(w, "Flags:\n")
	renderTable(w, "  ", leafFlagRows(c))
	if len(c.Examples) > 0 {
		fmt.Fprint(w, "\nExamples:\n")
		for _, ex := range c.Examples {
			fmt.Fprintf(w, "  %s\n", ex)
		}
	}
	if c.Footer != "" {
		fmt.Fprintf(w, "\n%s\n", c.Footer)
	}
}
