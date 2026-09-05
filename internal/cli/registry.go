package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// This file defines the CLI command registry: a data-driven description of
// the command tree (names, aliases, positional arity, flags, help text).
// It was built as the first step of the CLI hardening plan's registry
// migration - the tree built in
// registry_tree.go is currently UNUSED. Nothing in cli.go, main.go, or any
// dispatcher reads from this yet; that wiring happens in later steps so each
// step can be verified independently against the existing switch-based
// dispatcher before it is replaced.

// FlagKind identifies the Go type backing a FlagSpec.
type FlagKind int

const (
	FlagString FlagKind = iota
	FlagBool
	FlagInt
)

// FlagSpec describes one command-specific flag. Global flags (--server,
// --json, --username, --password) are not represented here - they are added
// by the dispatcher via newFlagSet for every command, per operational-
// interfaces.md 5.1/5.2.
type FlagSpec struct {
	Name  string
	Kind  FlagKind
	Usage string

	DefString string
	DefInt    int
	DefBool   bool

	// Required, if true, means the dispatcher must reject a run where this
	// flag was left at its zero value. RequiredMsg is the exact message to
	// print (e.g. "error: --driver and --identifier are required") - kept
	// as a single message per group of required flags rather than derived,
	// because today's wording spans multiple flags in one sentence (node
	// control accept) and must not silently reformat into something no
	// existing test or user expects.
	Required    bool
	RequiredMsg string

	// Hidden omits the flag from generated help/man output while still
	// accepting it on the command line (no current flag needs this; it
	// exists so a future internal/debug flag doesn't have to touch this
	// type again).
	Hidden bool
}

// ArgSpec describes one positional argument slot.
type ArgSpec struct {
	Name     string
	Optional bool
	Variadic bool
}

// RunFunc is the signature every leaf command's implementation satisfies
// once migrated onto the registry (later plan step - see registry_tree.go).
type RunFunc func(ctx *RunCtx) int

// RunCtx carries everything a migrated Run function needs: the parsed
// global flags, the validated positional arguments, the matched Command
// (so a shared impl like "runtime start|stop|restart" can branch on
// ctx.Cmd.Name), the command-specific flag values, and the output streams.
//
// Flag values are resolved into a plain map at parse time (Name -> value)
// rather than exposing the *flag.FlagSet directly: RunCtx's job is to be a
// simple, stable surface for command implementations, and a map keeps
// String/Bool/Int trivial and panic-free for callers that pass a flag name
// declared on the command.
type RunCtx struct {
	Flags *globalFlags
	Args  []string
	Cmd   *Command

	Stdout io.Writer
	Stderr io.Writer

	values  map[string]any
	visited map[string]bool
}

// String returns the string flag value for name, or "" if name was not
// declared as a FlagString on the matched command.
func (c *RunCtx) String(name string) string {
	if v, ok := c.values[name].(string); ok {
		return v
	}
	return ""
}

// Bool returns the bool flag value for name, or false if name was not
// declared as a FlagBool on the matched command.
func (c *RunCtx) Bool(name string) bool {
	if v, ok := c.values[name].(bool); ok {
		return v
	}
	return false
}

// Int returns the int flag value for name, or 0 if name was not declared as
// a FlagInt on the matched command.
func (c *RunCtx) Int(name string) int {
	if v, ok := c.values[name].(int); ok {
		return v
	}
	return 0
}

// IsSet reports whether the named flag was explicitly supplied on the
// command line (via fs.Visit). Used for PATCH semantics where only visited
// flags should be sent to the server.
func (c *RunCtx) IsSet(name string) bool {
	return c.visited[name]
}

// newRunCtx builds a RunCtx from a parsed FlagSet, pulling out each of cmd's
// declared FlagSpecs by name and kind. intFlags carries the *int pointers
// dispatch.go's registerCommandFlags got back from fs.Int for this same
// FlagSet's FlagInt-kind flags, keyed by name - reading through those
// pointers is a direct, already-parsed value with no string round-trip and
// no swallowed parse error (see registerCommandFlags's doc comment for why).
func newRunCtx(cmd *Command, fs *flag.FlagSet, intFlags map[string]*int, flags *globalFlags, args []string, stdout, stderr io.Writer) *RunCtx {
	values := make(map[string]any, len(cmd.Flags))
	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	for _, f := range cmd.Flags {
		switch f.Kind {
		case FlagString:
			if fv := fs.Lookup(f.Name); fv != nil {
				values[f.Name] = fv.Value.String()
			}
		case FlagBool:
			if fv := fs.Lookup(f.Name); fv != nil {
				values[f.Name] = fv.Value.String() == "true"
			}
		case FlagInt:
			if p, ok := intFlags[f.Name]; ok && p != nil {
				values[f.Name] = *p
			}
		}
	}
	return &RunCtx{Flags: flags, Args: args, Cmd: cmd, Stdout: stdout, Stderr: stderr, values: values, visited: visited}
}

// Command is a node in the CLI command tree. Metadata is data, not code, so
// the dispatcher, help writer, completion writer, man generator, and
// main.go's command whitelist can all derive from the same declaration
// instead of drifting independently (see plan context).
type Command struct {
	Name    string
	Aliases []string

	Short  string // one line, used in parent's command table and man
	Long   string // optional paragraph for man/--help
	Footer string // optional trailing note (e.g. auth requirement callout)

	Args  []ArgSpec
	Flags []FlagSpec
	Sub   []*Command

	Run RunFunc

	Hidden    bool // omitted from the root command table, still reachable
	NeedsAuth bool

	Examples []string

	parent *Command // set by finalize; unexported so it can't be hand-set inconsistently
}

// MinArgs returns the number of required positionals - the count of leading
// ArgSpecs with Optional == false. Derived, not stored, so it can never
// disagree with Args.
func (c *Command) MinArgs() int {
	n := 0
	for _, a := range c.Args {
		if a.Optional {
			break
		}
		n++
	}
	return n
}

// MaxArgs returns the maximum number of positionals, or -1 if the last
// ArgSpec is variadic (unbounded).
func (c *Command) MaxArgs() int {
	for _, a := range c.Args {
		if a.Variadic {
			return -1
		}
	}
	return len(c.Args)
}

// Path walks the parent chain and returns the full invocation path, e.g.
// "marbor models pull". Requires finalize to have run first (parent
// pointers set); before that it returns just c.Name. The root command's own
// Name ("marbor") supplies the leading token - callers must not also
// prepend it.
func (c *Command) Path() string {
	var parts []string
	for cur := c; cur != nil; cur = cur.parent {
		parts = append([]string{cur.Name}, parts...)
	}
	return strings.Join(parts, " ")
}

// UsageLine renders a one-line "usage: <path> <args> [flags]" string from
// Args, matching the hand-written usage strings already used throughout
// cli.go (e.g. "usage: marbor models pull <node> <model> [flags]").
func (c *Command) UsageLine() string {
	var b strings.Builder
	b.WriteString("usage: ")
	b.WriteString(c.Path())
	for _, a := range c.Args {
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
	// Every leaf implicitly accepts the global flags (--server/--json/
	// --username/--password) via newFlagSet regardless of whether it
	// declares any command-specific Flags of its own (see leafFlagRows in
	// help.go), so "[flags]" is always appended - matching nearly every
	// hand-written "usage: ..." string in the pre-registry cli.go, which
	// showed "[flags]" unconditionally (e.g. "models pull <node> <model>
	// [flags]" even though "pull" declares zero command-specific flags).
	b.WriteString(" [flags]")
	return b.String()
}

// lookup matches tok against this command's direct children by Name then by
// Aliases, returning nil if none match.
func (c *Command) lookup(tok string) *Command {
	for _, s := range c.Sub {
		if s.Name == tok {
			return s
		}
	}
	for _, s := range c.Sub {
		for _, alias := range s.Aliases {
			if alias == tok {
				return s
			}
		}
	}
	return nil
}

// boolFlagNames returns the set of flag names that must be treated as
// boolean (no value token follows them) when splitting flags from
// positionals - {"json"} unioned with any FlagBool-kind flag declared on
// this command. This replaces the hardcoded map[string]bool{"json": true}
// literal repeated at every call site in cli.go today.
func (c *Command) boolFlagNames() map[string]bool {
	names := map[string]bool{"json": true}
	for _, f := range c.Flags {
		if f.Kind == FlagBool {
			names[f.Name] = true
		}
	}
	return names
}

// TopLevelCommandNames returns the Name of every direct child of root() -
// the set of bare words that main.go's resolveCommand must route to "cli".
// Backed by the same registry that the dispatcher, help writer, and
// man/completion generators read, so main.go's whitelist cannot drift out of
// sync with the commands actually implemented in cli.Run (see registry_tree.go's
// buildRoot - this is what makes "key", "spill", and
// "requests" reachable from the real binary).
//
// Deliberately includes Hidden commands (e.g. "completion") - Hidden only
// controls whether a command is advertised in root --help, never whether it
// is dispatchable. Filtering hidden commands out here would silently
// reintroduce the exact unreachable-command bug this function exists to fix.
// Callers that want the advertised subset for display (root help) should
// filter root().Sub themselves, not rely on this function to do it.
func TopLevelCommandNames() []string {
	r := root()
	names := make([]string, 0, len(r.Sub))
	for _, s := range r.Sub {
		names = append(names, s.Name)
	}
	return names
}

// Root returns the full CLI command tree (registry_tree.go's root()),
// built exactly once. Exported so a different package - cmd/gen-docs, which
// generates man pages/docs/cli.md/the README CLI table and cannot call the
// unexported root() - can walk the same tree the dispatcher and help writer
// use, with parent pointers already set by finalize (so Path() and
// UsageLine() work correctly on every node reached from here).
func Root() *Command {
	return root()
}

// finalize walks the tree rooted at root, setting parent pointers, and
// panics with a clear message if the tree is malformed. It is meant to run
// exactly once, from inside a sync.OnceValue constructor (registry_tree.go)
// - so a malformed tree can only ever be caught on first CLI use in tests or
// at runtime, never silently at server start (server start does not touch
// this package's registry).
func finalize(root *Command) *Command {
	finalizeNode(root, nil)
	return root
}

func finalizeNode(c *Command, parent *Command) {
	c.parent = parent

	// Every leaf must have Run set; every non-leaf (Run == nil) must have
	// at least one Sub. Run is deliberately left nil for every command in
	// this plan step (the dispatcher isn't wired yet - see registry_tree.go
	// comment) via an explicit panic("not yet migrated") stub, which keeps
	// this invariant meaningful today and forces migration to be
	// intentional per command in later steps.
	if c.Run == nil && len(c.Sub) == 0 {
		panic(fmt.Sprintf("cli: command %q has no Run and no Sub - every leaf must have Run set", c.Path()))
	}

	seenNames := map[string]bool{}
	for _, s := range c.Sub {
		if seenNames[s.Name] {
			panic(fmt.Sprintf("cli: duplicate subcommand name %q under %q", s.Name, c.Path()))
		}
		seenNames[s.Name] = true
		for _, alias := range s.Aliases {
			if seenNames[alias] {
				panic(fmt.Sprintf("cli: duplicate subcommand name/alias %q under %q", alias, c.Path()))
			}
			seenNames[alias] = true
		}
	}

	seenOptional := false
	variadicSeen := false
	for i, a := range c.Args {
		if variadicSeen {
			panic(fmt.Sprintf("cli: command %q declares more than one variadic arg, or a variadic arg not in the last position", c.Path()))
		}
		if a.Variadic {
			variadicSeen = true
			if i != len(c.Args)-1 {
				panic(fmt.Sprintf("cli: command %q has a variadic arg %q that is not last", c.Path(), a.Name))
			}
		}
		if a.Optional {
			seenOptional = true
		} else if seenOptional {
			panic(fmt.Sprintf("cli: command %q has required arg %q after an optional arg", c.Path(), a.Name))
		}
	}

	for _, f := range c.Flags {
		if f.Usage == "" {
			panic(fmt.Sprintf("cli: command %q flag %q has empty Usage", c.Path(), f.Name))
		}
	}

	for _, s := range c.Sub {
		finalizeNode(s, c)
	}
}
