package main

import (
	"fmt"
	"strings"

	"github.com/Anirudhx7/marbor/internal/cli"
)

// nonregistry_render.go renders the commands listed in nonregistry.go into
// the Markdown reference and the man pages. They take their own flags, not the
// global Admin API flags, so none of those are listed here.

func writeMarkdownNonRegistry(b *strings.Builder, c nonRegistryCommand) {
	fmt.Fprintf(b, "### `%s`\n\n%s\n\n%s\n\nUsage: `%s`\n\n", c.Name, c.Short, c.Long, c.Usage)
	if len(c.Flags) > 0 {
		b.WriteString("Flags:\n\n")
		for _, f := range c.Flags {
			fmt.Fprintf(b, "- `%s` - %s\n", f.Signature, f.Usage)
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
}

// genNonRegistryMan renders one page, e.g. "marbor-bench.1".
func genNonRegistryMan(root *cli.Command, c nonRegistryCommand) string {
	var b strings.Builder
	writeManHeader(&b, slugFor(root, c.Name), c.Short)

	b.WriteString(".SH SYNOPSIS\n")
	fmt.Fprintf(&b, ".B %s\n", roffEscape(c.Usage))

	b.WriteString(".SH DESCRIPTION\n")
	b.WriteString(roffEscape(c.Long) + "\n")

	if len(c.Flags) > 0 {
		b.WriteString(".SH FLAGS\n")
		for _, f := range c.Flags {
			b.WriteString(".TP\n")
			fmt.Fprintf(&b, ".B %s\n%s\n", roffEscape(f.Signature), roffEscape(f.Usage))
		}
	}

	writeManExamples(&b, c.Examples)

	b.WriteString(".SH SEE ALSO\n")
	fmt.Fprintf(&b, "\\fB%s\\fR(1)\n", roffEscape(root.Name))
	return b.String()
}
