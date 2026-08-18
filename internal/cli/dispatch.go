package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// dispatch.go implements the generic, registry-driven command dispatcher -
// built as step 5 of the P83+ CLI hardening plan (see
// .local/plans/reflective-pondering-acorn.md, Implementation section 2).
// cli.go's Run now delegates directly to dispatchAndRun, which is the real,
// only dispatch path in production - the old hand-rolled switch this
// replaced has been deleted.
//
// dispatch() itself still never calls a matched command's Run function - see
// dispatchResult's doc comment; that is dispatchAndRun's job, once dispatch
// has fully validated the request. During migration, this separation let
// dispatch()'s walking/validation/help layer be exercised (via a since-
// deleted differential test, dispatch_differential_test.go, comparing it
// against the legacy switch across ~190 argument vectors) without ever
// invoking a leaf's Run, each of which was still a notYetMigrated stub that
// panicked. That test was removed once cli.Run started delegating directly
// to this dispatcher, since comparing the dispatcher against itself would
// have been tautological. Current coverage is dispatch_p83_test.go's
// registry-wide trailing-argument test plus the full internal/cli test
// suite.

// dispatchResult describes what dispatch() decided to do for one argument
// vector.
type dispatchResult struct {
	// code is the process exit code, meaningful only when handled is true.
	code int

	// handled reports whether dispatch fully handled the request itself
	// (help was printed, or an error was reported) - in that case the
	// caller does nothing further and just returns code.
	handled bool

	// matched is the leaf command that would run, set only when
	// handled == false.
	matched *Command

	// ctx is the fully validated RunCtx ready to pass to matched.Run, set
	// only when handled == false. dispatch never calls matched.Run(ctx)
	// itself - see dispatchAndRun.
	ctx *RunCtx
}

// helpTarget resolves the command whose help should actually be rendered
// for cur. This exists for exactly one legacy quirk: old cli.go's "node"
// top-level case (cli.go:474-491) always rendered "node control"'s own help
// (the probe/accept action table), never "node"'s own single-child table,
// for all four of "node" alone, "node -h", "node control" alone, and
// "node control -h" alike. Every other command renders its own help
// unchanged.
//
// Investigated (Fix 11 of the P83+ CLI hardening code review) whether this
// is really "node"-specific or actually a general "a group with exactly one
// child and no bare-execution Run of its own redirects --help straight to
// that child" rule. It is NOT: "requests" (registry_tree.go) is exactly such
// a group too - Run == nil, exactly one Sub ("explain") - but "requests
// --help"/"requests" bare invocation is documented and tested
// (testdata/help/requests.golden, via printRequestsUsage) to show the normal
// one-row action table ("Actions:\n  explain <request-id>  ..."), not jump
// straight to "explain"'s own leaf help. Generalizing the rule to "len(Sub)
// == 1 && Run == nil" would silently change "requests --help"'s real,
// currently-correct output the moment this file changed, for a group that
// was never affected by the legacy quirk being reproduced here. So the
// special case stays exactly "node"-specific rather than generalizing:
// "node" and "requests" are both single-child pure groups today, but only
// "node" has ever had this redirect behavior, and there is no registry-level
// signal (Hidden, NeedsAuth, anything) that distinguishes "collapse to my
// only child" groups from "show my own one-row action table" groups except
// the name itself. Verified "node --help"/"node -h"/"node control --help"
// output is unaffected by this investigation (no code changed here beyond
// this comment).
func helpTarget(cur *Command) *Command {
	if cur != nil && cur.Name == "node" && cur.parent != nil && cur.parent.parent == nil {
		if ctrl := cur.lookup("control"); ctrl != nil {
			return ctrl
		}
	}
	return cur
}

// siblingNamesOf returns the non-hidden direct child names of cur, in
// declaration order - the candidate list for both the oxford-joined "want
// ..." list and the suggest() typo-correction pass.
func siblingNamesOf(cur *Command) []string {
	names := make([]string, 0, len(cur.Sub))
	for _, s := range cur.Sub {
		if !s.Hidden {
			names = append(names, s.Name)
		}
	}
	return names
}

// oxfordJoin renders items as "a", "a or b", or "a, b, or c" - matching the
// three existing hand-written lists in cli.go byte-for-byte for the 4-item
// case ("pull, delete, unload, or list" at cli.go:391) and extending the
// same convention to 1- and 2-item lists that did not exist as hand-written
// strings before (new groups now covered by the same generic message; this
// was verified during migration against a differential test comparing
// against the legacy switch, since deleted once cli.Run delegated directly
// to this dispatcher - see dispatch.go's top-of-file comment for why).
func oxfordJoin(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
	}
}

// reportUnknownToken reports tok as an unmatched token under cur (which is
// either root itself, for a genuinely unknown top-level command, or some
// group node reached by walking, for an unknown subcommand/action) and
// writes the appropriate help below it. It always returns
// {code: ExitUserError, handled: true}.
//
// Message shape deliberately mirrors cli.go's existing hand-written
// messages when cur == root or cur is one of the groups that already had an
// "unknown X action" message (models/nodes/runtime/node control):
//   - root:  "unknown command %q"                                  (cli.go:576)
//   - group: "unknown <path-without-binary> action %q (want %s)"   (cli.go:334,391,470,523)
//
// For groups that had NO such message before (key, requests, node
// top-level - see cli.go:532-535, :561-563, :479-482, which fall straight to
// plain group help with no "unknown ..." line at all), this dispatcher
// deliberately now emits the same unified message. That is an intentional
// generalization, not a bug: it is the entire point of replacing four
// independently hand-rolled behaviors with one dispatch path (plan section 2,
// "all 14 sites now produce that string from one code path"). This was
// documented and pinned as an intentional difference during migration via a
// differential test comparing this dispatcher against the legacy switch
// across ~190 argument vectors, since deleted once cli.Run started
// delegating directly to this dispatcher (further comparison would have been
// tautological) - see dispatch.go's top-of-file comment.
//
// The "Did you mean %q?" suggestion line is new for every group (old cli.go
// had none at all) - explicitly one of the plan's called-out intentional
// additions (Implementation section 2: "Suggestions come from a ~25-line
// two-row stdlib Levenshtein...").
func reportUnknownToken(root, cur *Command, tok string, stdout, stderr io.Writer) dispatchResult {
	names := siblingNamesOf(cur)
	if cur == root {
		fmt.Fprintf(stderr, "unknown command %q\n", tok)
	} else {
		label := strings.TrimPrefix(cur.Path(), root.Name+" ")
		fmt.Fprintf(stderr, "unknown %s action %q (want %s)\n", label, tok, oxfordJoin(names))
	}
	if s := suggest(tok, names); len(s) > 0 {
		fmt.Fprintf(stderr, "Did you mean %q?\n", s[0])
	}
	fmt.Fprintln(stderr)
	if cur == root {
		writeHelp(stderr, root)
	} else {
		writeHelp(stderr, helpTarget(cur))
	}
	return dispatchResult{code: ExitUserError, handled: true}
}

// registerCommandFlags declares cur's own FlagSpecs on fs (in addition to
// the global flags newFlagSet already registered), so fs.Parse recognizes
// them and newRunCtx's later fs.Lookup(f.Name) calls succeed. The FlagString/
// FlagBool bound variables are intentionally discarded - RunCtx reads those
// values back out of fs by name via newRunCtx's fs.Lookup(f.Name).
//
// FlagInt is the one exception: it returns the map of the actual *int
// pointers fs.Int gave back for each FlagInt-kind flag, keyed by name, so
// newRunCtx can read the already-parsed int directly instead of re-parsing
// fv.Value.String() via fmt.Sscanf (which round-trips through a string for
// no reason and silently swallows a parse error that, by construction,
// fs.Parse would already have caught - flag.FlagSet's own *intValue wrapper
// type is unexported, so this map of pointers is how this package gets at
// the typed value without reaching into flag's internals).
func registerCommandFlags(fs *flag.FlagSet, flags []FlagSpec) map[string]*int {
	ints := make(map[string]*int)
	for _, f := range flags {
		switch f.Kind {
		case FlagString:
			fs.String(f.Name, f.DefString, f.Usage)
		case FlagBool:
			fs.Bool(f.Name, f.DefBool, f.Usage)
		case FlagInt:
			ints[f.Name] = fs.Int(f.Name, f.DefInt, f.Usage)
		}
	}
	return ints
}

// isZeroFlagValue reports whether fv (a *flag.Flag looked up by name) is
// still at a value indistinguishable from "not provided" - used only for
// Required checks today (the only Required flags declared in the registry as
// of this step: "nodes confirm-tls --fingerprint" and "node control accept
// --driver/--identifier", both FlagString).
//
// The zero-value check MUST be kind-specific: a FlagString flag's only
// "unset" representation is the empty string. Treating the literal strings
// "0" or "false" as "not provided" for a string flag is wrong - those are
// valid values a user can legitimately pass (e.g. a container/PID identifier
// literally named "0"), and doing so wrongly rejects them as missing. A
// FlagBool or FlagInt flag has no other way to represent "unset" via
// flag.FlagSet (a bool/int flag's string form is always "true"/"false" or a
// base-10 integer), so their zero-value check stays as-is; no Required
// FlagBool/FlagInt flag exists in the registry today, but the kind-specific
// switch below keeps this correct if one is ever added.
func isZeroFlagValue(kind FlagKind, fv *flag.Flag) bool {
	if fv == nil {
		return true
	}
	switch kind {
	case FlagString:
		return fv.Value.String() == ""
	case FlagBool, FlagInt:
		switch fv.Value.String() {
		case "", "0", "false":
			return true
		default:
			return false
		}
	default:
		return fv.Value.String() == ""
	}
}

// rootOf walks cur's parent chain to the tree's root.
func rootOf(cur *Command) *Command {
	for cur.parent != nil {
		cur = cur.parent
	}
	return cur
}

// dispatchRun handles a matched, runnable command (cur.Run != nil): builds
// its FlagSet (command-specific flags plus the global set), splits rest into
// flags/positionals, parses, validates arity and required flags, and - on
// success - builds the RunCtx and returns it unhandled so the caller decides
// whether to invoke cur.Run. It never calls cur.Run itself.
func dispatchRun(cur *Command, rest []string, stdout, stderr io.Writer) dispatchResult {
	// Old cli.go always named its FlagSet after the bare subcommand words
	// with no leading binary name (e.g. newFlagSet("models pull", stderr),
	// newFlagSet("spill", stderr)) - flag.FlagSet.Usage()/PrintDefaults
	// prints that name verbatim as "Usage of <name>:" on a bad-flag error,
	// so this must match exactly rather than using cur.Path() (which
	// includes the "ollama-mesh " prefix).
	fsName := strings.TrimPrefix(cur.Path(), rootOf(cur).Name+" ")
	fs, flags := newFlagSet(fsName, stderr)
	intFlags := registerCommandFlags(fs, cur.Flags)

	usageFn := func(w io.Writer) { writeHelp(w, helpTarget(cur)) }

	flagArgs, positional := splitFlagsAndArgs(rest, cur.boolFlagNames())

	if ok, code := parseFlags(fs, flagArgs, usageFn, stdout); !ok {
		return dispatchResult{code: code, handled: true}
	}

	// Belt and braces per the plan: append any leftover fs.Args() so a
	// disagreement between splitFlagsAndArgs' heuristic and flag's own
	// boundary detection can never let an extra positional slip through
	// unchecked.
	positional = append(positional, fs.Args()...)

	min, max := cur.MinArgs(), cur.MaxArgs()
	if len(positional) < min || (max >= 0 && len(positional) > max) {
		// Deliberate: no "error:" prefix here - this is the P83 arity
		// error, and ~10 existing tests assert the bare "usage: ..." line
		// with no prefix (see the plan's "Deliberate" note in
		// Implementation section 2). Missing-required-flag below keeps the
		// "error: " prefix intentionally, for the same reason in reverse.
		fmt.Fprintln(stderr, cur.UsageLine())
		return dispatchResult{code: ExitUserError, handled: true}
	}

	for _, f := range cur.Flags {
		if !f.Required {
			continue
		}
		if isZeroFlagValue(f.Kind, fs.Lookup(f.Name)) {
			msg := f.RequiredMsg
			if msg == "" {
				msg = fmt.Sprintf("error: --%s is required", f.Name)
			}
			fmt.Fprintln(stderr, msg)
			return dispatchResult{code: ExitUserError, handled: true}
		}
	}

	ctx := newRunCtx(cur, fs, intFlags, flags, positional, stdout, stderr)
	return dispatchResult{matched: cur, ctx: ctx, handled: false}
}

// dispatch walks root by consuming leading non-dash tokens that match a
// child (Command.lookup), then decides between: printing help, reporting an
// unknown command/subcommand, reporting a bare pure-group invocation as an
// error, or handing off to dispatchRun for arity/flag validation. See the
// plan's Implementation section 2 for the full error contract this
// implements.
func dispatch(root *Command, args []string, stdout, stderr io.Writer) dispatchResult {
	if len(args) == 0 {
		// Matches cli.go's old bare-invocation behavior: root help to
		// stderr, ExitUserError.
		writeHelp(stderr, root)
		return dispatchResult{code: ExitUserError, handled: true}
	}

	if args[0] == "help" {
		// Old cli.go's top-level switch treated the literal word "help" as
		// a synonym for "-h"/"--help" - hasHelpFlag only recognizes the two
		// dash-prefixed spellings, so this needs its own check, matching
		// only at the very first token (there is no "help" subcommand
		// anywhere else in the tree).
		writeHelp(stdout, root)
		return dispatchResult{code: ExitOK, handled: true}
	}

	cur := root
	i := 0
	for i < len(args) {
		tok := args[i]
		if strings.HasPrefix(tok, "-") {
			// Dash-guard: a token that looks like a flag is never treated
			// as an attempted subcommand match, so e.g. "models --json"
			// cannot be misread as looking for a subcommand named
			// "--json" (plan Implementation section 2, "Dash-guard
			// semantics" risk item).
			break
		}
		child := cur.lookup(tok)
		if child == nil {
			break
		}
		cur = child
		i++
	}
	rest := args[i:]

	if hasHelpFlag(rest) {
		if cur == root {
			// Root --help now goes through the same registry-driven
			// writeHelp/writeRootHelp path as every group/leaf, instead of
			// the old hand-written `usage` const in cli.go (deleted - see
			// Fix 3 of the P83+ CLI hardening code review). This keeps root
			// help aligned via renderTable's tabwriter and prevents it from
			// drifting out of sync with the registry as commands are added.
			writeHelp(stdout, root)
		} else {
			writeHelp(stdout, helpTarget(cur))
		}
		return dispatchResult{code: ExitOK, handled: true}
	}

	if cur == root {
		// rest is guaranteed non-empty here: args is non-empty (checked
		// above) and cur can only still be root if the walk loop never
		// descended, i.e. rest == args. Old cli.go's top-level switch
		// (cli.go:280) has no dash guard at all - it switches on args[0]
		// literally - so even a stray flag like "ollama-mesh --bogus"
		// falls to the "unknown command" default case, not just an
		// unmatched word.
		return reportUnknownToken(root, root, rest[0], stdout, stderr)
	}

	if len(cur.Sub) > 0 {
		unmatched := false
		if len(rest) > 0 {
			if cur.Run != nil {
				// Bare-executable group (models, nodes): only a non-dash
				// leftover token is a failed subcommand attempt. A dash
				// token is real flag input for the bare execution and
				// falls through to dispatchRun below - replicates the
				// explicit "!strings.HasPrefix(rest[0], \"-\")" guard old
				// cli.go used for exactly these two groups (cli.go:314,344).
				unmatched = !strings.HasPrefix(rest[0], "-")
			} else {
				// Pure group, no bare execution (runtime, key, requests,
				// node/control): there is no fallback interpretation for a
				// leftover token, dash or not, so it is always reported as
				// an unknown action - see reportUnknownToken's doc comment
				// for why this unifies (rather than replicates) four
				// different legacy behaviors here.
				unmatched = true
			}
		}
		if unmatched {
			return reportUnknownToken(root, cur, rest[0], stdout, stderr)
		}
	}

	if len(rest) == 0 && cur.Run == nil {
		// Pure group invoked completely bare (e.g. "runtime", "key",
		// "requests", "node", "node control") - matches each of these
		// groups' current bare-invocation behavior (cli.go:402,532,561,
		// 479-482,483-491) of an ExitUserError help dump to stderr.
		// helpTarget collapses "node" to "node control"'s help - see its
		// doc comment.
		writeHelp(stderr, helpTarget(cur))
		return dispatchResult{code: ExitUserError, handled: true}
	}

	// cur.Run != nil here: either a bare-executable group with no unmatched
	// leftover token (models/nodes invoked with zero args, or with only
	// dash-prefixed flag args), or a plain leaf command (version, status,
	// login, models pull, runtime drain, node control accept, ...).
	return dispatchRun(cur, rest, stdout, stderr)
}

// dispatchAndRun is the convenience wrapper a later migration step will wire
// into cli.Run in place of (part of) its switch statement - see the plan's
// migration order, step 6. It is dead code as of this step: nothing calls
// it from cli.Run yet, and it only exists so dispatch's validation layer can
// be exercised end-to-end (including the actual matched.Run call) once a
// command's Run is no longer the notYetMigrated stub, e.g. from a future
// test that stubs a single command's Run deliberately.
func dispatchAndRun(root *Command, args []string, stdout, stderr io.Writer) int {
	result := dispatch(root, args, stdout, stderr)
	if result.handled {
		return result.code
	}
	return result.matched.Run(result.ctx)
}
