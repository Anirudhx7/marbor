package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/cli"
)

var (
	// helpRowRe matches one hand-listed row of cmd/marbor/main.go's
	// helpTableRows: the full usage text and the command word.
	helpRowRe = regexp.MustCompile(`\{"(marbor ([A-Za-z0-9_-]+)[^"]*)"`)

	// dispatchCaseRe matches one `case "word", ...:` label of resolveCommand.
	dispatchCaseRe = regexp.MustCompile(`(?m)^\tcase ((?:"[^"]+"(?:, )?)+):`)

	// flagDeclRe matches a flag declaration on a FlagSet: kind, name, default
	// and usage literal, e.g. fs.String("target", "http://...", "usage").
	flagDeclRe = regexp.MustCompile(`\bfs\.([A-Za-z0-9]+)\(\s*"([^"]+)"\s*,\s*([^,]+?)\s*,\s*("(?:[^"\\]|\\.)*")\s*\)`)

	// durationDefaultRe reads a duration default written as N*time.Unit.
	durationDefaultRe = regexp.MustCompile(`^(\d+)\*time\.(Millisecond|Second|Minute|Hour)$`)

	// quotedWordRe picks the words out of a dispatch case label list.
	quotedWordRe = regexp.MustCompile(`"([^"]+)"`)

	// fsCallRe matches every method call on a FlagSet variable named fs.
	// The receiver must be the bare identifier fs, not a field or another
	// variable ending in fs (opts.fs.String, myfs.String).
	fsCallRe = regexp.MustCompile(`(?m)(?:^|[^.\w])fs\.([A-Za-z0-9]+)\(`)

	// exampleFlagRe picks the --flag tokens out of an example command line. A
	// flag starts a word, so a "--" inside another token is not one.
	exampleFlagRe = regexp.MustCompile(`(?:^|\s)--([A-Za-z][A-Za-z0-9-]*)`)
)

// fsNonDeclaration lists the FlagSet methods that are not flag declarations;
// any other method call on fs counts as a declaration and must be parsed.
var fsNonDeclaration = map[string]bool{
	"Parse": true, "SetOutput": true, "PrintDefaults": true, "Args": true,
	"NArg": true, "Arg": true, "NFlag": true, "Set": true, "Lookup": true,
	"Visit": true, "VisitAll": true, "Parsed": true, "Name": true, "Output": true,
}

var durationUnits = map[string]time.Duration{
	"Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
}

// repoRoot is the repository root relative to this package's directory, which
// is where go test runs. readRepoFile must not be used after a Chdir.
const repoRoot = "../.."

func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{repoRoot}, rel...)...))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(rel...), err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// sectionBetween returns the source text from the first occurrence of start
// to the next closing brace at line start, failing the test when either is
// missing so a reformatted source cannot make a check pass vacuously.
func sectionBetween(t *testing.T, src, start string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("cmd/marbor/main.go no longer contains %q - update this test", start)
	}
	j := strings.Index(src[i:], "\n}")
	if j < 0 {
		t.Fatalf("could not find the end of %q in cmd/marbor/main.go", start)
	}
	return src[i : i+j]
}

func documentedNames() map[string]nonRegistryCommand {
	out := map[string]nonRegistryCommand{}
	for _, c := range nonRegistryCommands {
		out[c.Name] = c
	}
	return out
}

// TestNonRegistry_HelpTableMatches fails when a hand-listed subcommand in the
// binary's help table is missing from nonRegistryCommands, is documented with
// a different usage line, or when a documented command is no longer listed.
func TestNonRegistry_HelpTableMatches(t *testing.T) {
	src := readRepoFile(t, "cmd", "marbor", "main.go")
	rows := helpRowRe.FindAllStringSubmatch(sectionBetween(t, src, "\nvar helpTableRows"), -1)

	documented := documentedNames()
	seen := map[string]bool{}
	for _, m := range rows {
		usage, name := m[1], m[2]
		if seen[name] {
			t.Errorf("`marbor %s` is listed more than once in the binary's help table", name)
		}
		seen[name] = true
		c, ok := documented[name]
		if !ok {
			t.Errorf("`marbor %s` is in the binary's help table but missing from nonRegistryCommands, so the generated reference omits it", name)
			continue
		}
		if c.Usage != usage {
			t.Errorf("%s: documented usage %q differs from the help table row %q", name, c.Usage, usage)
		}
	}
	if len(seen) == 0 {
		t.Fatal("parsed zero subcommand rows from helpTableRows - the row format changed, update this test")
	}
	for name := range documented {
		if !seen[name] {
			t.Errorf("%s is in nonRegistryCommands but not in the binary's help table", name)
		}
	}
}

// TestNonRegistry_DispatchMatches fails when resolveCommand dispatches a new
// bare subcommand word that is neither a documented non-registry command nor
// one of the known special words.
func TestNonRegistry_DispatchMatches(t *testing.T) {
	src := readRepoFile(t, "cmd", "marbor", "main.go")
	body := sectionBetween(t, src, "\nfunc resolveCommand(")
	cases := dispatchCaseRe.FindAllStringSubmatch(body, -1)
	if len(cases) == 0 {
		t.Fatal("parsed zero case labels from resolveCommand - update this test")
	}
	// help words and the removed "agent" notice are not documented commands.
	special := map[string]bool{"help": true, "-h": true, "--help": true, "agent": true}
	documented := documentedNames()
	dispatched := map[string]bool{}
	for _, m := range cases {
		for _, w := range quotedWordRe.FindAllStringSubmatch(m[1], -1) {
			word := w[1]
			if special[word] {
				continue
			}
			dispatched[word] = true
			if _, ok := documented[word]; !ok {
				t.Errorf("resolveCommand dispatches %q but nonRegistryCommands does not document it", word)
			}
		}
	}
	for name := range documented {
		if !dispatched[name] {
			t.Errorf("%s is documented but resolveCommand does not dispatch it", name)
		}
	}
}

// TestNonRegistry_NoRegistryCollision fails when a command moves into (or is
// added to) the registry while still listed as a non-registry command, which
// would generate it twice.
func TestNonRegistry_NoRegistryCollision(t *testing.T) {
	root := cli.Root()
	for _, c := range nonRegistryCommands {
		if w := strings.Fields(c.Usage); len(w) < 2 || w[0] != root.Name || w[1] != c.Name {
			t.Errorf("%s: usage %q must start with the words %q", c.Name, c.Usage, root.Name+" "+c.Name)
		}
		for _, r := range root.Sub {
			if r.Name == c.Name {
				t.Errorf("%q is both a registry command and in nonRegistryCommands", c.Name)
			}
		}
	}
}

// expectedFlagType and expectedDefault translate a parsed flag declaration
// into what the reference must show. An unknown kind or default form fails the
// test so the guard is extended rather than silently skipped.
func expectedFlagType(t *testing.T, cmd, flagName, kind string) string {
	t.Helper()
	switch kind {
	case "String":
		return " string"
	case "Int":
		return " int"
	case "Duration":
		return " duration"
	case "Bool":
		return ""
	}
	t.Errorf("%s: flag --%s uses fs.%s, which this test does not know how to document", cmd, flagName, kind)
	return ""
}

func expectedDefault(t *testing.T, cmd, flagName, kind, def string) string {
	t.Helper()
	switch kind {
	case "String":
		v, err := strconv.Unquote(def)
		if err != nil {
			t.Errorf("%s: cannot read the default of --%s (%s): %v", cmd, flagName, def, err)
			return ""
		}
		if v == "" {
			return ""
		}
		return " (default " + strconv.Quote(v) + ")"
	case "Duration":
		m := durationDefaultRe.FindStringSubmatch(def)
		if m == nil {
			t.Errorf("%s: cannot read the duration default of --%s (%s)", cmd, flagName, def)
			return ""
		}
		n, _ := strconv.Atoi(m[1])
		return " (default " + (time.Duration(n) * durationUnits[m[2]]).String() + ")"
	case "Bool":
		if def == "false" {
			return ""
		}
		if def == "true" {
			return " (default true)"
		}
	case "Int":
		if def == "0" {
			return ""
		}
		if _, err := strconv.Atoi(def); err == nil {
			return " (default " + def + ")"
		}
	}
	t.Errorf("%s: cannot read the default of --%s (%s)", cmd, flagName, def)
	return ""
}

// flagSources returns, per non-registry command, the non-test Go files that
// may declare its flags.
func flagSources(t *testing.T) map[string]string {
	t.Helper()
	benchDir := filepath.Join(repoRoot, "internal", "bench")
	entries, err := os.ReadDir(benchDir)
	if err != nil {
		t.Fatalf("reading %s: %v", benchDir, err)
	}
	var bench strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			bench.WriteString(readRepoFile(t, "internal", "bench", e.Name()))
		}
	}
	return map[string]string{
		"bench":     bench.String(),
		"uninstall": readRepoFile(t, "cmd", "marbor", "uninstall.go"),
	}
}

// TestNonRegistry_FlagsMatchSource fails when a flag of bench or uninstall is
// added, removed, retyped, re-defaulted or re-described in its source without
// the reference entry changing with it.
func TestNonRegistry_FlagsMatchSource(t *testing.T) {
	// Flags are read from FlagSet variables named fs, the form both commands
	// use; a flag declared on another variable or on flag.CommandLine would
	// not be seen, which the completeness count below cannot catch.
	sources := flagSources(t)
	for _, c := range nonRegistryCommands {
		src, ok := sources[c.Name]
		if !ok {
			t.Errorf("no source file registered for non-registry command %q in this test", c.Name)
			continue
		}
		decls := flagDeclRe.FindAllStringSubmatch(src, -1)
		if len(decls) == 0 {
			t.Errorf("%s: parsed zero flag declarations from its source - update this test", c.Name)
			continue
		}
		// Every flag declared anywhere in the file must be recognised, so a
		// declaration form the parser misses cannot hide a new flag.
		n := 0
		for _, m := range fsCallRe.FindAllStringSubmatch(src, -1) {
			if !fsNonDeclaration[m[1]] {
				n++
			}
		}
		if n != len(decls) {
			t.Errorf("%s: source has %d flag declaration call(s) on fs but only %d parsed - update flagDeclRe", c.Name, n, len(decls))
		}

		documented := map[string]nonRegistryFlag{}
		for _, f := range c.Flags {
			if _, dup := documented[f.Name]; dup {
				t.Errorf("%s: flag --%s is documented twice", c.Name, f.Name)
			}
			documented[f.Name] = f
		}
		for _, ex := range c.Examples {
			for _, m := range exampleFlagRe.FindAllStringSubmatch(ex, -1) {
				if _, ok := documented[m[1]]; !ok {
					t.Errorf("%s: example %q uses --%s, which is not a documented flag", c.Name, ex, m[1])
				}
			}
		}
		declared := map[string]bool{}
		for _, d := range decls {
			kind, name, def := d[1], d[2], d[3]
			usage, err := strconv.Unquote(d[4])
			if err != nil {
				t.Errorf("%s: cannot read the usage of --%s: %v", c.Name, name, err)
				continue
			}
			declared[name] = true
			f, ok := documented[name]
			if !ok {
				t.Errorf("%s: flag --%s is declared in the source but missing from nonRegistryCommands", c.Name, name)
				continue
			}
			wantSig := "--" + name + expectedFlagType(t, c.Name, name, kind)
			if f.Signature != wantSig {
				t.Errorf("%s: --%s is documented as %q, the source declares %q", c.Name, name, f.Signature, wantSig)
			}
			wantUsage := usage + expectedDefault(t, c.Name, name, kind, def)
			if f.Usage != wantUsage {
				t.Errorf("%s: --%s usage is documented as %q, the source declares %q", c.Name, name, f.Usage, wantUsage)
			}
		}
		var stale []string
		for name := range documented {
			if !declared[name] {
				stale = append(stale, name)
			}
		}
		sort.Strings(stale)
		for _, name := range stale {
			t.Errorf("%s: documented flag --%s is not declared in its source", c.Name, name)
		}
	}
}

// TestNonRegistry_AppearsInGeneratedOutput asserts the markdown reference,
// the root man page, the per-command man page and the README table carry
// every non-registry command with its flags, description and examples.
func TestNonRegistry_AppearsInGeneratedOutput(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seedReadme(t, dir)
	files := runGeneration(t, dir)

	md := string(files["docs/cli.md"])
	rootMan := string(files["docs/man/marbor.1"])
	readme := string(files["README.md"])
	for _, c := range nonRegistryCommands {
		// An empty Short or Long would make the Contains checks below pass
		// for any output.
		if c.Short == "" || c.Long == "" {
			t.Errorf("%s: Short and Long must both be non-empty", c.Name)
		}
		if !strings.Contains(md, "### `"+c.Name+"`\n") {
			t.Errorf("docs/cli.md has no section for %q", c.Name)
		}
		if !strings.Contains(md, "Usage: `"+c.Usage+"`") {
			t.Errorf("docs/cli.md does not show the usage line %q", c.Usage)
		}
		for _, f := range c.Flags {
			if !strings.Contains(md, "- `"+f.Signature+"` - "+f.Usage+"\n") {
				t.Errorf("docs/cli.md does not list flag %q of %q", f.Signature, c.Name)
			}
		}
		for _, ex := range c.Examples {
			if !strings.Contains(md, ex+"\n") {
				t.Errorf("docs/cli.md does not show example %q of %q", ex, c.Name)
			}
		}
		if !strings.Contains(rootMan, ".B "+c.Name+"\n") || !strings.Contains(rootMan, `\fBmarbor-`+c.Name+`\fR(1)`) {
			t.Errorf("root man page does not list %q and link to its page", c.Name)
		}
		page, ok := files["docs/man/marbor-"+c.Name+".1"]
		if !ok {
			t.Errorf("no man page generated for %q", c.Name)
			continue
		}
		for _, want := range []string{".SH SYNOPSIS", ".SH DESCRIPTION", ".SH EXAMPLES", ".SH SEE ALSO", roffEscape(c.Usage)} {
			if !strings.Contains(string(page), want) {
				t.Errorf("man page for %q is missing %q", c.Name, want)
			}
		}
		for _, f := range c.Flags {
			if !strings.Contains(string(page), ".B "+roffEscape(f.Signature)+"\n"+roffEscape(f.Usage)+"\n") {
				t.Errorf("man page for %q does not list flag %q with its usage", c.Name, f.Signature)
			}
		}
		for _, want := range []string{c.Long, c.Short} {
			if !strings.Contains(md, want) {
				t.Errorf("docs/cli.md does not carry the text %q of %q", want, c.Name)
			}
			if !strings.Contains(string(page), roffEscape(want)) {
				t.Errorf("man page for %q does not carry the text %q", c.Name, want)
			}
		}
		for _, ex := range c.Examples {
			if !strings.Contains(string(page), roffEscape(ex)+"\n") {
				t.Errorf("man page for %q does not show example %q", c.Name, ex)
			}
		}
		if !strings.Contains(readme, "| `"+c.Usage+"` | "+c.Short+" |") {
			t.Errorf("README CLI table has no row for %q", c.Usage)
		}
	}
}

// TestGenerated_CommittedDocsAreCurrent fails when the committed reference
// differs from what the generator produces from the current registry, or when
// a stale man page is left behind, so the check also runs under go test and
// not only in CI's drift step.
func TestGenerated_CommittedDocsAreCurrent(t *testing.T) {
	readme := readRepoFile(t, "README.md")
	committed := map[string]string{"README.md": readme, "docs/cli.md": readRepoFile(t, "docs", "cli.md")}
	manDir := filepath.Join(repoRoot, "docs", "man")
	entries, err := os.ReadDir(manDir)
	if err != nil {
		t.Fatalf("reading %s: %v", manDir, err)
	}
	for _, e := range entries {
		committed["docs/man/"+e.Name()] = readRepoFile(t, "docs", "man", e.Name())
	}

	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("README.md", []byte(readme), 0o644); err != nil {
		t.Fatalf("copying README.md: %v", err)
	}
	generated := runGeneration(t, dir)

	for name, want := range generated {
		got, ok := committed[name]
		if !ok {
			t.Errorf("%s is generated but not committed - run `make docs`", name)
			continue
		}
		if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
			t.Errorf("%s is out of date - run `make docs`", name)
		}
	}
	for name := range committed {
		if _, ok := generated[name]; !ok {
			t.Errorf("%s is committed but no longer generated - delete it", name)
		}
	}
}

// TestNonRegistry_ParserPrecision pins the extraction regexes against the
// near-miss inputs they must not match, so a loosened pattern cannot make the
// drift checks pass on the wrong text.
func TestNonRegistry_ParserPrecision(t *testing.T) {
	got := exampleFlagRe.FindAllStringSubmatch("marbor bench --target x a--b -- --n 3", -1)
	if len(got) != 2 || got[0][1] != "target" || got[1][1] != "n" {
		t.Errorf("exampleFlagRe matched %v, want only --target and --n", got)
	}
	calls := fsCallRe.FindAllStringSubmatch("fs.String(\"a\")\n\tx := opts.fs.Int(\"b\")\n\tmyfs.Bool(\"c\")\n\tfs.Parse(args)", -1)
	if len(calls) != 2 {
		t.Errorf("fsCallRe matched %v, want only the two bare fs calls", calls)
	}
}
