package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// helpRowRe matches one hand-listed row of cmd/marbor/main.go's
// helpTableRows, e.g. {"marbor bench [flags]", "..."}.
var helpRowRe = regexp.MustCompile(`\{"marbor ([a-z][a-z-]*)[ "\[]`)

func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(rel...), err)
	}
	return string(data)
}

// TestNonRegistry_CoversEveryHandListedCommand fails when a subcommand is
// added to the binary's hand-listed help rows (the commands dispatched before
// the CLI registry) without a generated reference entry.
func TestNonRegistry_CoversEveryHandListedCommand(t *testing.T) {
	src := readRepoFile(t, "cmd", "marbor", "main.go")
	start := strings.Index(src, "var helpTableRows")
	if start < 0 {
		t.Fatal("cmd/marbor/main.go no longer declares helpTableRows - update this test to find the hand-listed commands")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of helpTableRows in cmd/marbor/main.go")
	}
	matches := helpRowRe.FindAllStringSubmatch(src[start:start+end], -1)
	if len(matches) == 0 {
		t.Fatal("parsed zero subcommand rows from helpTableRows - the row format changed, update this test")
	}

	listed := map[string]bool{}
	for _, c := range nonRegistryCommands {
		listed[c.Name] = true
	}
	for _, m := range matches {
		if !listed[m[1]] {
			t.Errorf("`marbor %s` is in the binary's help table but missing from nonRegistryCommands, so the generated reference omits it", m[1])
		}
	}
	if len(matches) != len(nonRegistryCommands) {
		t.Errorf("helpTableRows lists %d subcommands, nonRegistryCommands has %d", len(matches), len(nonRegistryCommands))
	}
}

// TestNonRegistry_FlagsMatchSource fails when a flag of bench or uninstall is
// renamed, added, removed or re-described in the command's source without the
// reference entry changing with it.
func TestNonRegistry_FlagsMatchSource(t *testing.T) {
	sources := map[string]string{
		"bench":     readRepoFile(t, "internal", "bench", "bench.go"),
		"uninstall": readRepoFile(t, "cmd", "marbor", "uninstall.go"),
	}
	declRe := regexp.MustCompile(`fs\.(?:String|Bool|Duration|Int)\("([a-z-]+)"`)
	for _, c := range nonRegistryCommands {
		src, ok := sources[c.Name]
		if !ok {
			t.Errorf("no source file registered for non-registry command %q in this test", c.Name)
			continue
		}
		declared := map[string]bool{}
		for _, m := range declRe.FindAllStringSubmatch(src, -1) {
			declared[m[1]] = true
		}
		if len(declared) == 0 {
			t.Fatalf("parsed zero flag declarations from the %s source - update this test", c.Name)
		}
		documented := map[string]bool{}
		for _, f := range c.Flags {
			documented[f.Name] = true
			if !declared[f.Name] {
				t.Errorf("%s: documented flag --%s is not declared in its source", c.Name, f.Name)
			}
			// Usage text is the source's own string, optionally followed by
			// a " (default ...)" suffix the flag package prints.
			usage := f.Usage
			if i := strings.LastIndex(usage, " (default "); i >= 0 {
				usage = usage[:i]
			}
			if !strings.Contains(src, `"`+usage+`"`) {
				t.Errorf("%s: usage text of --%s does not match the source: %q", c.Name, f.Name, usage)
			}
		}
		for name := range declared {
			if !documented[name] {
				t.Errorf("%s: flag --%s is declared in the source but missing from nonRegistryCommands", c.Name, name)
			}
		}
	}
}

// TestNonRegistry_AppearsInGeneratedOutput asserts the markdown reference,
// the root man page, a per-command man page and the README table all carry
// every non-registry command.
func TestNonRegistry_AppearsInGeneratedOutput(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seedReadme(t, dir)
	files := runGeneration(t, dir)

	for _, c := range nonRegistryCommands {
		md := string(files["docs/cli.md"])
		if !strings.Contains(md, "### `"+c.Name+"`") {
			t.Errorf("docs/cli.md has no section for %q", c.Name)
		}
		if !strings.Contains(md, c.Usage) {
			t.Errorf("docs/cli.md does not show the usage line %q", c.Usage)
		}
		for _, f := range c.Flags {
			if !strings.Contains(md, "`"+f.Signature+"`") {
				t.Errorf("docs/cli.md does not list flag %q of %q", f.Signature, c.Name)
			}
		}
		root := string(files["docs/man/marbor.1"])
		if !strings.Contains(root, ".B "+c.Name+"\n") {
			t.Errorf("root man page COMMANDS does not list %q", c.Name)
		}
		page, ok := files["docs/man/marbor-"+c.Name+".1"]
		if !ok {
			t.Errorf("no man page generated for %q", c.Name)
		} else if !strings.Contains(string(page), ".SH SYNOPSIS") {
			t.Errorf("man page for %q has no SYNOPSIS", c.Name)
		}
		if !strings.Contains(string(files["README.md"]), "`"+c.Usage+"`") {
			t.Errorf("README CLI table has no row for %q", c.Usage)
		}
	}
}
