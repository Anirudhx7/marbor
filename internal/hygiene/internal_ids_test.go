// Package hygiene holds repo-wide text hygiene checks that don't belong to
// any single production package - standing guards over what the whole
// public repo (source, comments, docs) is allowed to say, not what the code
// does.
package hygiene

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// internalIDPattern catches internal-only references that must never ship
// in this public repo: queue/ticket IDs (P411, P-A2-09b), guard citations
// (R8, R10), Law references (Law 6), LESSONS refs (L23), and any literal
// path into this project's private, non-public planning directory (a repo
// convention, not itself sensitive - the point is that a public clone
// doesn't contain that directory, so a comment citing a path into it points
// a reader at a file they can never open). Case-sensitive on purpose -
// lowercase "p50"/"p95"/"p99" (latency percentiles) are legitimate product
// vocabulary, not ticket IDs.
//
// Deliberately generic: this file does NOT enumerate the private
// directory's actual document names. A pattern that had to spell those out
// to catch bare (unpathed) mentions of them would itself become the one
// place in the public repo permanently cataloging that internal structure
// once every other scattered reference is cleaned up - the opposite of
// what this guard exists to prevent. The accepted gap is a bare mention of
// a private doc with no path prefix at all; catch those by direct review
// during the sweep, not by hardcoding names here.
//
// This is the repo-wide sibling of internal/cli's
// TestRegistry_NoInternalIDLeakage (which only walks CLI help strings).
// That test stays as the fast, precise CLI-specific check; this one is the
// broader net directed 2026-09-05 (Anirudh) after P415 was found to also
// leak into README.md, CHANGELOG.md, docs/*.md, and Go/TS comments
// repo-wide, not just --help text, and to also leak private-directory path
// references (found in internal/cli/registry.go, then confirmed via
// `git grep` across 43 tracked files) - not just queue numbers.
var internalIDPattern = regexp.MustCompile(
	`\bP-?[A-Z0-9]*-?\d{2,}[a-z]?\b` +
		`|\bR\d{1,2}\b` +
		`|\bLaw\s*\d+\b` +
		`|\bL\d{2,}\b` +
		`|\.local/\S+`,
)

// internalIDExceptions lists exact matched substrings that look like
// internal IDs to the pattern above but are real product terms. Add an
// entry here (with a one-line reason) rather than weakening the pattern -
// it is the documented, reviewable escape hatch for a genuine false
// positive (e.g. a GPU model number), not a way to silence a real finding.
var internalIDExceptions = map[string]string{
	"P40":  "NVIDIA Tesla P40 GPU model name",
	"P100": "NVIDIA Tesla P100 GPU model name",
}

// scanExtensions are the file extensions this test reads. Anything else
// tracked in git (images, lockfiles, .sum hashes, binary fixtures) is
// skipped - it is not prose or code a human reads for meaning.
var scanExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".md": true, ".yml": true, ".yaml": true, ".sh": true, ".ps1": true,
	".sql": true, ".golden": true, ".txt": true, ".html": true, ".json": true,
}

// selfExemptFiles are guard-test files that necessarily discuss the ID
// pattern itself as their subject matter (e.g. this file, and
// internal/cli/registry_test.go's own doc comments) - scanning them would
// be the test flagging its own documentation of what it looks for.
var selfExemptFiles = map[string]bool{
	"internal/hygiene/internal_ids_test.go": true,
	"internal/cli/registry_test.go":         true,
}

// gitTrackedFiles returns every file tracked in the repo (absolute paths)
// via `git ls-files`, restricted to scanExtensions and excluding
// selfExemptFiles.
func gitTrackedFiles(t *testing.T) []string {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	rootDir := strings.TrimSpace(string(root))

	out, err := exec.Command("git", "-C", rootDir, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var files []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		p := string(raw)
		if selfExemptFiles[p] {
			continue
		}
		dot := strings.LastIndex(p, ".")
		if dot < 0 || !scanExtensions[p[dot:]] {
			continue
		}
		files = append(files, rootDir+"/"+p)
	}
	return files
}

// TestNoInternalIDsRepoWide is the standing guard directed 2026-09-05
// (Anirudh, expanding P415's original CLI-only scope): every tracked
// source/doc file in the public repo, including comments, must be free of
// internal-only queue-item IDs, guard/Law citations, and LESSONS refs.
// Historical git commits are NOT rewritten (accepted paper trail per the
// same directive) - this only guards what ships going forward.
//
// If this test is failing, the fix is rewording the flagged line in plain
// language (or adding a documented exception to internalIDExceptions for a
// genuine false positive), not touching this test.
func TestNoInternalIDsRepoWide(t *testing.T) {
	files := gitTrackedFiles(t)
	if len(files) == 0 {
		t.Fatal("gitTrackedFiles returned nothing - check the git invocation, not the pattern")
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			for _, m := range internalIDPattern.FindAllString(line, -1) {
				if _, ok := internalIDExceptions[m]; ok {
					continue
				}
				t.Errorf("%s:%s: internal-only reference %q in line: %q", path, strconv.Itoa(i+1), m, strings.TrimSpace(line))
			}
		}
	}
}
