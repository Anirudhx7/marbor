package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ollama-mesh/ollama-mesh/internal/cli"
)

// markdown.go renders docs/cli.md: one "##" section per top-level command,
// "###"/"####"/... for nested subcommands (heading level tracks tree depth),
// covering the same content as the man pages in Markdown form for the repo's
// docs/ directory.

func generateMarkdown(root *cli.Command) error {
	var b strings.Builder
	b.WriteString("# ollama-mesh CLI Reference\n\n")
	b.WriteString("Generated from the CLI command registry (`internal/cli`) by `cmd/gen-docs` - " +
		"do not edit by hand; run `make docs` after changing the registry.\n\n")

	b.WriteString("## Global flags\n\n")
	b.WriteString("Every command accepts these in addition to any flags listed under it:\n\n")
	for _, f := range globalFlags {
		fmt.Fprintf(&b, "- `--%s` - %s\n", f.Name, f.Usage)
	}
	b.WriteString("\n")

	b.WriteString("## Exit status\n\n")
	fmt.Fprintf(&b, "- `%d` - success\n", cli.ExitOK)
	fmt.Fprintf(&b, "- `%d` - user error (bad arguments, unknown command, validation failure)\n", cli.ExitUserError)
	fmt.Fprintf(&b, "- `%d` - server error (the Admin API is unreachable or returned an unexpected error)\n", cli.ExitServerError)
	b.WriteString("- `3` - reserved for future partial-success reporting (batch operations); unused today\n")
	fmt.Fprintf(&b, "- `%d` - authentication error (missing, invalid, or expired credentials)\n\n", cli.ExitAuthError)

	b.WriteString("## Environment\n\n")
	b.WriteString("- `MESH_SERVER` - Admin API base URL, used when `--server` is not given\n")
	b.WriteString("- `MESH_USERNAME` - admin username, used when `--username` is not given\n")
	b.WriteString("- `MESH_PASSWORD` - admin password, used when `--password` is not given\n\n")

	b.WriteString("## Files\n\n")
	b.WriteString("The session saved by `ollama-mesh login` (mode `0600`), under the OS user " +
		"config dir - e.g. `~/.config/ollama-mesh/session` on Linux, " +
		"`~/Library/Application Support/ollama-mesh/session` on macOS, " +
		"`%AppData%\\ollama-mesh\\session` on Windows.\n\n")

	b.WriteString("## Commands\n\n")
	for _, c := range root.Sub {
		writeMarkdownCommand(&b, c, 3)
	}

	return os.WriteFile("docs/cli.md", []byte(b.String()), 0644)
}

func writeMarkdownCommand(b *strings.Builder, c *cli.Command, level int) {
	heading := strings.Repeat("#", level)
	fmt.Fprintf(b, "%s `%s%s`\n\n", heading, c.Name, argsSuffix(c.Args))

	if c.Hidden {
		b.WriteString("_Hidden from `--help` output, but fully reachable._\n\n")
	}
	if c.Short != "" {
		fmt.Fprintf(b, "%s\n\n", c.Short)
	}
	if c.Long != "" {
		fmt.Fprintf(b, "%s\n\n", c.Long)
	}
	if c.NeedsAuth {
		b.WriteString("Requires authentication - see the root README's CLI auth section, or run `ollama-mesh login`.\n\n")
	}

	if flags := visibleFlags(c); len(flags) > 0 {
		b.WriteString("Flags:\n\n")
		for _, f := range flags {
			req := ""
			if f.Required {
				req = " (required)"
			}
			fmt.Fprintf(b, "- `%s` - %s%s\n", flagSignature(f), f.Usage, req)
		}
		b.WriteString("\n")
	}

	if len(c.Examples) > 0 {
		b.WriteString("Examples:\n\n```bash\n")
		for _, ex := range c.Examples {
			b.WriteString(ex + "\n")
		}
		b.WriteString("```\n\n")
	}

	if c.Footer != "" {
		fmt.Fprintf(b, "%s\n\n", c.Footer)
	}

	for _, sub := range c.Sub {
		writeMarkdownCommand(b, sub, level+1)
	}
}
