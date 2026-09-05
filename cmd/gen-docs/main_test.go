package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/cli"
)

// seedReadme writes a minimal README.md carrying the CLI table markers into
// dir, so generateReadmeTable has something to edit - mirrors the real
// README.md's marker placement without depending on its full content.
func seedReadme(t *testing.T, dir string) {
	t.Helper()
	content := "# Title\n\nSome prose that stays untouched.\n\n" +
		"## CLI\n\n" +
		readmeBeginMarker + "\n" +
		"| Command | Purpose |\n|---|---|\n| `old` | stale placeholder row |\n" +
		readmeEndMarker + "\n\n" +
		"More prose that stays untouched.\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0644); err != nil {
		t.Fatalf("seeding README.md: %v", err)
	}
}

// runGeneration runs all three generators against the live registry inside
// dir (relative-path writers, so the caller must have already chdir'd
// there) and returns a map of relative path -> file contents for every
// generated file, for byte-identical comparison across repeated runs.
func runGeneration(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	root := cli.Root()

	if err := generateManPages(root); err != nil {
		t.Fatalf("generateManPages: %v", err)
	}
	if err := generateMarkdown(root); err != nil {
		t.Fatalf("generateMarkdown: %v", err)
	}
	if err := generateReadmeTable(root); err != nil {
		t.Fatalf("generateReadmeTable: %v", err)
	}

	out := map[string][]byte{}
	manDir := filepath.Join(dir, "docs", "man")
	entries, err := os.ReadDir(manDir)
	if err != nil {
		t.Fatalf("reading %s: %v", manDir, err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(manDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		out["docs/man/"+e.Name()] = data
	}

	cliMD, err := os.ReadFile(filepath.Join(dir, "docs", "cli.md"))
	if err != nil {
		t.Fatalf("reading docs/cli.md: %v", err)
	}
	out["docs/cli.md"] = cliMD

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	out["README.md"] = readme

	return out
}

// TestGenerate_Deterministic runs generation twice into the same temp dir
// and asserts byte-identical output - the required determinism guarantee:
// running gen-docs twice must never
// produce a diff, or the CI drift check would be permanently red.
func TestGenerate_Deterministic(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seedReadme(t, dir)

	first := runGeneration(t, dir)
	second := runGeneration(t, dir)

	if len(first) != len(second) {
		t.Fatalf("file set changed between runs: first=%d files, second=%d files", len(first), len(second))
	}
	for name, want := range first {
		got, ok := second[name]
		if !ok {
			t.Fatalf("%s present in first run, missing in second", name)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs between two generation runs (not deterministic)", name)
		}
	}
}

// TestGenerate_RootManSections asserts the root man page contains every
// expected section header and that generation over the live registry never
// panics.
func TestGenerate_RootManSections(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seedReadme(t, dir)

	files := runGeneration(t, dir)
	root, ok := files["docs/man/marbor.1"]
	if !ok {
		t.Fatal("docs/man/marbor.1 was not generated")
	}
	page := string(root)

	wantSections := []string{
		".SH NAME", ".SH SYNOPSIS", ".SH DESCRIPTION", ".SH COMMANDS",
		".SH OPTIONS", ".SH EXIT STATUS", ".SH ENVIRONMENT", ".SH FILES",
		".SH SEE ALSO",
	}
	for _, s := range wantSections {
		if !strings.Contains(page, s) {
			t.Errorf("root man page missing section %q", s)
		}
	}
}

// TestGenerate_GroupPagesExist asserts a page was generated for every
// top-level command that has subcommands, matching groupPageCommands.
func TestGenerate_GroupPagesExist(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seedReadme(t, dir)

	files := runGeneration(t, dir)
	root := cli.Root()
	for _, g := range groupPageCommands(root) {
		name := "docs/man/" + pageSlug(root, g) + ".1"
		if _, ok := files[name]; !ok {
			t.Errorf("expected group man page %s was not generated", name)
		}
	}
}

// TestGenerate_ReadmeMarkersRequired asserts a missing marker fails loudly
// instead of silently appending or guessing a location.
func TestGenerate_ReadmeMarkersRequired(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Title\n\nNo markers here.\n"), 0644); err != nil {
		t.Fatalf("seeding README.md: %v", err)
	}

	err := generateReadmeTable(cli.Root())
	if err == nil {
		t.Fatal("expected an error when README.md has no CLI table markers, got nil")
	}
	if !strings.Contains(err.Error(), readmeBeginMarker) {
		t.Errorf("error message %q does not mention the missing marker", err)
	}
}
