package cli

import (
	"fmt"
	"io"
	"strings"
)

// completion.go generates static shell completion scripts (bash, zsh, fish)
// for the "marbor completion <shell>" command (registry_tree.go's
// completionCmd()) - P83+ CLI hardening plan, Implementation section 7. Every
// script is built by walking the CURRENT command registry tree
// (registry.go/registry_tree.go) at generation time and baking command,
// subcommand, and flag names into plain shell text.
//
// Deliberately no dynamic behavior: no network call, no Admin API request, no
// authentication, ever. A completion script that requires marbor to be
// reachable would defeat its own purpose (tab-completing "marbor
// runtime start <TAB>" must still work when marbor is down or the operator
// isn't logged in).
//
// Determinism: every walk below iterates root.Sub (and nested Sub) in
// declared order - never map iteration - so calling any of the three
// generators twice on the same tree produces byte-identical output
// (TestRun_Completion_Deterministic pins this).

// completionNode is one entry in a flattened, declaration-ordered walk of the
// command tree - the shared traversal all three generators use so they can
// never disagree on ordering or on which commands are included.
type completionNode struct {
	path []string // e.g. ["models", "pull"]; nil for root itself
	cmd  *Command
}

// walkCompletionTree returns root and every non-hidden descendant, in
// declaration order. Hidden commands (and anything nested under one) are
// omitted - completion for the "completion" command itself is not offered,
// matching Hidden's existing "omitted from the root table" contract.
func walkCompletionTree(root *Command) []completionNode {
	var nodes []completionNode
	var walk func(c *Command, path []string)
	walk = func(c *Command, path []string) {
		nodes = append(nodes, completionNode{path: path, cmd: c})
		for _, s := range c.Sub {
			if s.Hidden {
				continue
			}
			childPath := make([]string, len(path)+1)
			copy(childPath, path)
			childPath[len(path)] = s.Name
			walk(s, childPath)
		}
	}
	walk(root, nil)
	return nodes
}

// visibleChildren returns c's non-hidden direct children, declaration order.
func visibleChildren(c *Command) []*Command {
	var out []*Command
	for _, s := range c.Sub {
		if !s.Hidden {
			out = append(out, s)
		}
	}
	return out
}

// ownFlagNames returns "--name" for each non-hidden FlagSpec declared
// directly on c (not the global auth flags), declaration order.
func ownFlagNames(c *Command) []string {
	names := make([]string, 0, len(c.Flags))
	for _, f := range c.Flags {
		if f.Hidden {
			continue
		}
		names = append(names, "--"+f.Name)
	}
	return names
}

// globalFlagNames are the flags every command accepts via newFlagSet
// (cli.go) - never represented as a Command.Flags entry, so every generator
// below adds them explicitly.
var globalFlagNames = []string{"--server", "--json", "--username", "--password"}

// globalValueFlagNames is the subset of globalFlagNames that takes a value
// (i.e. is not boolean) - "--json" is the only boolean global flag.
var globalValueFlagNames = []string{"--server", "--username", "--password"}

// allValueFlagNames returns every non-boolean flag name declared anywhere in
// the tree, plus the global value flags, in first-seen order. Used by the
// bash generator's "don't try to complete a flag's value" guard, which is
// deliberately tree-wide rather than per-command context - the plan calls
// for keeping this reasonably simple, not exact per-command precision.
func allValueFlagNames(root *Command) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, n := range globalValueFlagNames {
		add(n)
	}
	for _, node := range walkCompletionTree(root) {
		for _, f := range node.cmd.Flags {
			if f.Hidden || f.Kind == FlagBool {
				continue
			}
			add("--" + f.Name)
		}
	}
	return names
}

// escapeSingleQuotes makes s safe to embed inside a single-quoted shell
// string literal (bash/zsh/fish all use the same escaping: close the quote,
// emit an escaped literal quote, reopen the quote).
func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// ---------------------------------------------------------------------------
// bash
// ---------------------------------------------------------------------------

// generateBashCompletion builds a _marbor() completion function using
// COMP_WORDS/COMP_CWORD to reconstruct the command path typed so far, then a
// case "$cmd" in ... esac with one arm per non-hidden command path in the
// tree, each running compgen -W "<children> <flags> <global flags>" against
// the current word.
func generateBashCompletion(root *Command) string {
	nodes := walkCompletionTree(root)

	var b strings.Builder
	fmt.Fprintf(&b, "# bash completion for %s\n", root.Name)
	b.WriteString("# generated from the command registry - do not edit by hand.\n\n")
	b.WriteString("_marbor() {\n")
	b.WriteString("    local cur cmd i word\n")
	b.WriteString("    cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("    cmd=\"\"\n")
	b.WriteString("    for (( i = 1; i < COMP_CWORD; i++ )); do\n")
	b.WriteString("        word=\"${COMP_WORDS[i]}\"\n")
	b.WriteString("        case \"$word\" in\n")
	b.WriteString("            -*) ;;\n")
	b.WriteString("            *)\n")
	b.WriteString("                if [[ -n \"$cmd\" ]]; then cmd=\"$cmd $word\"; else cmd=\"$word\"; fi\n")
	b.WriteString("                ;;\n")
	b.WriteString("        esac\n")
	b.WriteString("    done\n\n")

	// Value-flag guard: never try to complete the value of a flag that
	// takes one - just return no suggestions for that word.
	if valueFlags := allValueFlagNames(root); len(valueFlags) > 0 {
		b.WriteString("    if (( COMP_CWORD > 0 )); then\n")
		fmt.Fprintf(&b, "        case \"${COMP_WORDS[COMP_CWORD-1]}\" in\n")
		fmt.Fprintf(&b, "            %s) COMPREPLY=(); return 0 ;;\n", strings.Join(valueFlags, "|"))
		b.WriteString("        esac\n")
		b.WriteString("    fi\n\n")
	}

	b.WriteString("    case \"$cmd\" in\n")
	for _, node := range nodes {
		key := strings.Join(node.path, " ")
		words := make([]string, 0)
		for _, s := range visibleChildren(node.cmd) {
			words = append(words, s.Name)
		}
		words = append(words, ownFlagNames(node.cmd)...)
		words = append(words, globalFlagNames...)
		fmt.Fprintf(&b, "    %s)\n", bashCasePattern(key))
		fmt.Fprintf(&b, "        COMPREPLY=( $(compgen -W %s -- \"$cur\") )\n", shellSingleQuote(strings.Join(words, " ")))
		b.WriteString("        ;;\n")
	}
	b.WriteString("    *)\n")
	b.WriteString("        COMPREPLY=()\n")
	b.WriteString("        ;;\n")
	b.WriteString("    esac\n")
	b.WriteString("}\n")
	b.WriteString("complete -F _marbor marbor\n")
	return b.String()
}

// bashCasePattern renders key ("" for root, "models pull" for a nested
// command) as a case-statement pattern: a bare empty pattern is not legal
// bash, so root's key is rendered as the quoted empty string.
func bashCasePattern(key string) string {
	if key == "" {
		return `"")`
	}
	return shellSingleQuote(key) + ")"
}

// shellSingleQuote wraps s in single quotes, escaping any embedded single
// quote - safe for bash, zsh, and fish alike since all three share this
// escaping convention.
func shellSingleQuote(s string) string {
	return "'" + escapeSingleQuotes(s) + "'"
}

// ---------------------------------------------------------------------------
// zsh
// ---------------------------------------------------------------------------

// generateZshCompletion builds a #compdef marbor script using the
// standard _arguments -C '1:command:->cmd' '*::arg:->args' pattern, then
// dispatches on the reconstructed command path (same technique as bash,
// zsh-flavored) into per-group _describe calls listing child names with
// their Short text as descriptions, plus a flat _describe of every flag as
// '--name[description]'.
func generateZshCompletion(root *Command) string {
	nodes := walkCompletionTree(root)
	fnName := strings.ReplaceAll(root.Name, "-", "_")

	var b strings.Builder
	fmt.Fprintf(&b, "#compdef %s\n\n", root.Name)
	fmt.Fprintf(&b, "# zsh completion for %s\n", root.Name)
	b.WriteString("# generated from the command registry - do not edit by hand.\n\n")
	fmt.Fprintf(&b, "_%s() {\n", fnName)
	b.WriteString("    local context state state_descr line\n")
	b.WriteString("    typeset -A opt_args\n\n")
	b.WriteString("    _arguments -C \\\n")
	b.WriteString("        '1:command:->cmd' \\\n")
	b.WriteString("        '*::arg:->args'\n\n")

	b.WriteString("    local cmd=\"\"\n")
	b.WriteString("    local i word\n")
	b.WriteString("    for (( i = 2; i < CURRENT; i++ )); do\n")
	b.WriteString("        word=\"${words[i]}\"\n")
	b.WriteString("        case \"$word\" in\n")
	b.WriteString("            -*) ;;\n")
	b.WriteString("            *)\n")
	b.WriteString("                if [[ -n \"$cmd\" ]]; then cmd=\"$cmd $word\"; else cmd=\"$word\"; fi\n")
	b.WriteString("                ;;\n")
	b.WriteString("        esac\n")
	b.WriteString("    done\n\n")

	b.WriteString("    case \"$cmd\" in\n")
	for _, node := range nodes {
		children := visibleChildren(node.cmd)
		if len(children) == 0 {
			continue
		}
		key := strings.Join(node.path, " ")
		fmt.Fprintf(&b, "    %s)\n", bashCasePattern(key))
		b.WriteString("        local -a items\n")
		b.WriteString("        items=(\n")
		for _, s := range children {
			fmt.Fprintf(&b, "            '%s:%s'\n", escapeSingleQuotes(s.Name), escapeSingleQuotes(s.Short))
		}
		b.WriteString("        )\n")
		b.WriteString("        _describe 'command' items\n")
		b.WriteString("        ;;\n")
	}
	b.WriteString("    esac\n\n")

	b.WriteString("    local -a flags\n")
	b.WriteString("    flags=(\n")
	for _, l := range zshFlagLines(root) {
		fmt.Fprintf(&b, "        %s\n", l)
	}
	b.WriteString("    )\n")
	b.WriteString("    _describe -o 'option' flags\n")
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "_%s \"$@\"\n", fnName)
	return b.String()
}

// zshFlagLines renders every flag in the tree (global flags first, then
// each command's own, first-seen order, deduplicated by name) as a zsh
// _arguments-style spec: '--name[description]'.
func zshFlagLines(root *Command) []string {
	var lines []string
	seen := map[string]bool{}
	add := func(name, desc string) {
		if seen[name] {
			return
		}
		seen[name] = true
		lines = append(lines, fmt.Sprintf("'--%s[%s]'", name, escapeSingleQuotes(desc)))
	}
	add("server", "Admin API base URL")
	add("json", "output machine-readable JSON instead of a human table")
	add("username", "admin username, used to log in")
	add("password", "admin password, used to log in")
	for _, node := range walkCompletionTree(root) {
		for _, f := range node.cmd.Flags {
			if f.Hidden {
				continue
			}
			add(f.Name, f.Usage)
		}
	}
	return lines
}

// ---------------------------------------------------------------------------
// fish
// ---------------------------------------------------------------------------

// generateFishCompletion builds flat "complete -c marbor ..." lines:
// top-level subcommands gated on __fish_use_subcommand, nested subcommands
// gated on __fish_seen_subcommand_from <immediate parent>, and one line per
// flag (with -r for a flag that takes a value).
func generateFishCompletion(root *Command) string {
	nodes := walkCompletionTree(root)

	var b strings.Builder
	fmt.Fprintf(&b, "# fish completion for %s\n", root.Name)
	b.WriteString("# generated from the command registry - do not edit by hand.\n\n")

	for _, node := range nodes {
		children := visibleChildren(node.cmd)
		if len(children) == 0 {
			continue
		}
		if len(node.path) == 0 {
			for _, s := range children {
				fmt.Fprintf(&b, "complete -c %s -n '__fish_use_subcommand' -a %s -d %s\n",
					root.Name, shellSingleQuote(s.Name), shellSingleQuote(s.Short))
			}
			continue
		}
		parent := node.cmd.Name
		for _, s := range children {
			// parent is interpolated inside an already single-quoted
			// '__fish_seen_subcommand_from %s' string, so it goes through
			// escapeSingleQuotes (which is safe to embed there) rather than
			// shellSingleQuote (which would add its own enclosing quotes
			// and break the surrounding literal).
			fmt.Fprintf(&b, "complete -c %s -n '__fish_seen_subcommand_from %s' -a %s -d %s\n",
				root.Name, escapeSingleQuotes(parent), shellSingleQuote(s.Name), shellSingleQuote(s.Short))
		}
	}

	b.WriteString("\n")
	addFlag := func(name, desc string, valueTaking bool) {
		if valueTaking {
			fmt.Fprintf(&b, "complete -c %s -l %s -r -d %s\n", root.Name, name, shellSingleQuote(desc))
		} else {
			fmt.Fprintf(&b, "complete -c %s -l %s -d %s\n", root.Name, name, shellSingleQuote(desc))
		}
	}
	seen := map[string]bool{}
	emit := func(name, desc string, valueTaking bool) {
		if seen[name] {
			return
		}
		seen[name] = true
		addFlag(name, desc, valueTaking)
	}
	emit("server", "Admin API base URL", true)
	emit("json", "output machine-readable JSON instead of a human table", false)
	emit("username", "admin username, used to log in", true)
	emit("password", "admin password, used to log in", true)
	for _, node := range nodes {
		for _, f := range node.cmd.Flags {
			if f.Hidden {
				continue
			}
			emit(f.Name, f.Usage, f.Kind != FlagBool)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// dispatcher entry point
// ---------------------------------------------------------------------------

// runCompletion is completionCmd()'s Run function (registry_tree.go). The
// positional arity (exactly one) is already enforced by dispatchRun before
// this is ever called, so ctx.Args[0] is always present.
//
// It resolves the tree root via rootOf(ctx.Cmd) (dispatch.go) rather than
// calling root() directly: root() is the package-level var whose initializer
// (sync.OnceValue(buildRoot)) transitively constructs completionCmd(), which
// sets this very function as its Run - referencing the "root" identifier
// from here would create an initialization cycle the Go compiler rejects
// (root -> buildRoot -> completionCmd -> runCompletion -> root). rootOf just
// walks ctx.Cmd's already-resolved parent chain, so it carries no such
// dependency.
func runCompletion(ctx *RunCtx) int {
	shell := ctx.Args[0]
	treeRoot := rootOf(ctx.Cmd)

	var script string
	switch shell {
	case "bash":
		script = generateBashCompletion(treeRoot)
	case "zsh":
		script = generateZshCompletion(treeRoot)
	case "fish":
		script = generateFishCompletion(treeRoot)
	default:
		fmt.Fprintf(ctx.Stderr, "error: unknown shell %q (want bash, zsh, or fish)\n", shell)
		return ExitUserError
	}

	// Nothing else may write to stdout in the success path - completions
	// must be pipeable/sourceable (source <(marbor completion bash)).
	io.WriteString(ctx.Stdout, script)
	return ExitOK
}
