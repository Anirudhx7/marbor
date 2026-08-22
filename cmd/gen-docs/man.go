package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Anirudhx7/marbor/internal/cli"
)

// man.go renders the roff (man(7)) pages: one root page (docs/man/marbor.1)
// plus one page per top-level command that has subcommands (docs/man/marbor-<name>.1).

// flagLikeRe matches a leading "-" or "--" immediately followed by a letter,
// e.g. "--driver" or "-h". Escaping hyphens as "\-" only inside flag-like
// tokens (not every ordinary English hyphen in prose - e.g. "warm-state")
// keeps generated source readable while still giving man(1) flags a literal,
// non-breaking minus sign instead of a hyphen some renderers treat as a
// soft line-break point.
var flagLikeRe = regexp.MustCompile(`--?[A-Za-z][A-Za-z0-9-]*`)

// roffEscape prepares free text for embedding in roff source: backslashes
// first (so later substitutions can't introduce new ones that get
// double-escaped), then flag-like hyphens, then a per-line check for a
// leading "." or "'" - roff treats a line starting with either as a control
// line, which generated prose describing a command must never accidentally
// become.
func roffEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\e`)
	s = flagLikeRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "--") {
			return `\-\-` + m[2:]
		}
		return `\-` + m[1:]
	})
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, ".") || strings.HasPrefix(l, "'") {
			lines[i] = `\&` + l
		}
	}
	return strings.Join(lines, "\n")
}

func writeManHeader(b *strings.Builder, title, purpose string) {
	fmt.Fprintf(b, ".TH %s 1 \"%s\" \"marbor %s\" \"Marbor Manual\"\n", strings.ToUpper(roffEscape(title)), docsDate, cli.Version)
	b.WriteString(".SH NAME\n")
	fmt.Fprintf(b, "%s \\- %s\n", roffEscape(title), roffEscape(purpose))
}

func writeManExamples(b *strings.Builder, examples []string) {
	if len(examples) == 0 {
		return
	}
	b.WriteString(".SH EXAMPLES\n")
	for _, ex := range examples {
		b.WriteString(".PP\n.nf\n")
		b.WriteString(roffEscape(ex) + "\n")
		b.WriteString(".fi\n")
	}
}

// manifestName is the release-asset manifest listing exactly which man page
// filenames this run generated - see Fix 4 of the P83+ CLI hardening code
// review. install.sh (running on an end user's machine, with no Go toolchain
// and no access to the registry) fetches this file from a release FIRST and,
// if that succeeds, uses it to know which other man page files to fetch,
// instead of relying on its own hardcoded MAN_PAGES list. That hardcoded list
// stays only as a fallback for older releases published before this file
// existed. Written deterministically (declaration order, one filename per
// line) so two consecutive generator runs produce a byte-identical manifest.
const manifestName = "MANIFEST.txt"

func generateManPages(root *cli.Command) error {
	if err := os.MkdirAll(filepath.Join("docs", "man"), 0755); err != nil {
		return err
	}

	rootPage := root.Name + ".1"
	if err := os.WriteFile(filepath.Join("docs", "man", rootPage), []byte(genRootMan(root)), 0644); err != nil {
		return err
	}
	names := []string{rootPage}
	for _, g := range groupPageCommands(root) {
		name := pageSlug(root, g) + ".1"
		path := filepath.Join("docs", "man", name)
		if err := os.WriteFile(path, []byte(genGroupMan(root, g)), 0644); err != nil {
			return err
		}
		names = append(names, name)
	}

	manifest := strings.Join(names, "\n") + "\n"
	if err := os.WriteFile(filepath.Join("docs", "man", manifestName), []byte(manifest), 0644); err != nil {
		return err
	}
	return nil
}

func genRootMan(root *cli.Command) string {
	var b strings.Builder
	writeManHeader(&b, root.Name, root.Short)

	b.WriteString(".SH SYNOPSIS\n")
	fmt.Fprintf(&b, ".B %s\n.I command\n[\\fIflags\\fR]\n", roffEscape(root.Name))

	b.WriteString(".SH DESCRIPTION\n")
	b.WriteString(roffEscape(
		"Marbor is a single static binary that is three tools in one - the marbor "+
			"server, a marbor agent, and this thin CLI client of the Admin API - selected "+
			"by the first argument. As a CLI, every subcommand below is exactly one Admin "+
			"API request; it never talks to a marbor agent directly.") + "\n")

	groups := groupPageCommands(root)
	groupSet := map[string]bool{}
	for _, g := range groups {
		groupSet[g.Name] = true
	}

	b.WriteString(".SH COMMANDS\n")
	for _, c := range root.Sub {
		b.WriteString(".TP\n")
		fmt.Fprintf(&b, ".B %s%s\n", roffEscape(c.Name), roffEscape(argsSuffix(c.Args)))
		desc := roffEscape(c.Short)
		if groupSet[c.Name] {
			desc += fmt.Sprintf(". See \\fB%s\\fR(1).", pageSlug(root, c))
		}
		b.WriteString(desc + "\n")
	}

	b.WriteString(".SH OPTIONS\n")
	for _, f := range globalFlags {
		b.WriteString(".TP\n")
		fmt.Fprintf(&b, ".B \\-\\-%s\n%s\n", f.Name, roffEscape(f.Usage))
	}

	b.WriteString(".SH EXIT STATUS\n")
	fmt.Fprintf(&b, ".TP\n.B %d\nSuccess.\n", cli.ExitOK)
	fmt.Fprintf(&b, ".TP\n.B %d\nUser error (bad arguments, unknown command, validation failure).\n", cli.ExitUserError)
	fmt.Fprintf(&b, ".TP\n.B %d\nServer error (the Admin API is unreachable or returned an unexpected error).\n", cli.ExitServerError)
	b.WriteString(".TP\n.B 3\nReserved for future partial-success reporting (batch operations); unused today.\n")
	fmt.Fprintf(&b, ".TP\n.B %d\nAuthentication error (missing, invalid, or expired credentials).\n", cli.ExitAuthError)

	b.WriteString(".SH ENVIRONMENT\n")
	b.WriteString(".TP\n.B MARBOR_SERVER\nAdmin API base URL, used when \\-\\-server is not given.\n")
	b.WriteString(".TP\n.B MARBOR_USERNAME\nAdmin username, used when \\-\\-username is not given.\n")
	b.WriteString(".TP\n.B MARBOR_PASSWORD\nAdmin password, used when \\-\\-password is not given.\n")

	b.WriteString(".SH FILES\n")
	b.WriteString(".TP\n.B $XDG_CONFIG_HOME/marbor/session\n")
	b.WriteString(roffEscape(
		"The session saved by \"marbor login\" (mode 0600), under the OS user "+
			"config dir - e.g. ~/.config/marbor/session on Linux, "+
			"~/Library/Application Support/marbor/session on macOS, "+
			`%AppData%\marbor\session on Windows.`) + "\n")

	writeManExamples(&b, allExamples(root))

	b.WriteString(".SH SEE ALSO\n")
	seeAlso := make([]string, 0, len(groups))
	for _, g := range groups {
		seeAlso = append(seeAlso, fmt.Sprintf("\\fB%s\\fR(1)", pageSlug(root, g)))
	}
	b.WriteString(strings.Join(seeAlso, ",\n") + "\n")

	return b.String()
}

// genGroupMan renders one command-group page (e.g. "marbor-models.1"),
// flattening arbitrarily nested subcommands (e.g. "node control probe") into
// one COMMANDS list.
func genGroupMan(root *cli.Command, cmd *cli.Command) string {
	var b strings.Builder
	title := pageSlug(root, cmd)
	writeManHeader(&b, title, cmd.Short)

	b.WriteString(".SH SYNOPSIS\n")
	fmt.Fprintf(&b, ".B %s %s\n[action] [args] [\\fIflags\\fR]\n", roffEscape(root.Name), roffEscape(cmd.Name))

	b.WriteString(".SH DESCRIPTION\n")
	desc := cmd.Long
	if desc == "" {
		desc = cmd.Short
	}
	b.WriteString(roffEscape(desc) + "\n")

	pathPrefix := root.Name + " " + cmd.Name + " "
	b.WriteString(".SH COMMANDS\n")
	for _, d := range flattenDescendants(cmd) {
		rel := strings.TrimPrefix(d.Path(), pathPrefix)
		b.WriteString(".TP\n")
		fmt.Fprintf(&b, ".B %s%s\n", roffEscape(rel), roffEscape(argsSuffix(d.Args)))
		b.WriteString(roffEscape(d.Short) + "\n")

		if flags := visibleFlags(d); len(flags) > 0 {
			b.WriteString(".RS\n")
			for _, f := range flags {
				b.WriteString(".TP\n")
				fmt.Fprintf(&b, ".B %s\n", roffEscape(flagSignature(f)))
				usage := roffEscape(f.Usage)
				if f.Required {
					usage += " (required)"
				}
				b.WriteString(usage + "\n")
			}
			b.WriteString(".RE\n")
		}
	}

	// needsAuth is true if the group itself or any descendant declares
	// NeedsAuth - most groups (models, runtime, key, requests) set it on
	// the top-level command, but "node" sets it only on the nested
	// "control" node (registry_tree.go), not on "node" itself. Checking the
	// whole subtree means the auth flags still get documented once here
	// instead of being silently dropped for that one page's shape.
	needsAuth := cmd.NeedsAuth
	for _, d := range flattenDescendants(cmd) {
		if d.NeedsAuth {
			needsAuth = true
			break
		}
	}

	ownFlags := visibleFlags(cmd)
	if len(ownFlags) > 0 || needsAuth {
		b.WriteString(".SH FLAGS\n")
		for _, f := range ownFlags {
			b.WriteString(".TP\n")
			fmt.Fprintf(&b, ".B %s\n%s\n", roffEscape(flagSignature(f)), roffEscape(f.Usage))
		}
		if needsAuth {
			for _, gf := range globalFlags {
				b.WriteString(".TP\n")
				fmt.Fprintf(&b, ".B \\-\\-%s\n%s\n", gf.Name, roffEscape(gf.Usage))
			}
		}
	}

	writeManExamples(&b, allExamples(cmd))

	b.WriteString(".SH SEE ALSO\n")
	fmt.Fprintf(&b, "\\fB%s\\fR(1)\n", roffEscape(root.Name))

	return b.String()
}
