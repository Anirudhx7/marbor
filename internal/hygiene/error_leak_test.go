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

// errorLeakSinkPattern matches a call believed to write directly to the
// client's http.ResponseWriter with an error-shaped message:
// writeJSONError, http.Error, or an inline json.Encode of an "error" field.
// internal/admin's safe convention is writeServerError/writeCorrelatedError
// (see internal/admin/admin.go), which logs the real error server-side with
// a correlation ID and sends the client only a generic message + that ID -
// never err.Error(). Neither of those two safe functions matches this
// pattern, so calling through them never trips this guard.
var errorLeakSinkPattern = regexp.MustCompile(`writeJSONError\(|http\.Error\(|Encode\(map\[string\]string\{"error"`)

// rawErrRefPattern matches a bare `err` identifier reference (word
// boundary) - this project's conventional error variable name. A sink line
// that also references `err` is passing the real error (via err.Error(),
// %v/%s formatting, or similar) to the client instead of a safe message.
var rawErrRefPattern = regexp.MustCompile(`\berr\b`)

// errorLeakExceptions lists file:line pairs of known raw-error-to-client
// leaks, found while wiring this test (2026-09-15) via manual audit of
// internal/admin. These are the same bug class as commit 9aff1df ("stop
// leaking raw dial/TLS errors to cloud-test and probe clients") - just not
// all fixed yet. Reported separately as its own finding for per-site
// severity review (an error leaked to an authenticated admin-only endpoint
// is lower severity than one leaked to an unauthenticated path) and its own
// reviewed fix commit(s), rather than bundled into wiring this guard.
//
// Do not add a new entry here without a documented reason - a genuine new
// leak found by this test should be fixed by routing through
// writeServerError/writeCorrelatedError, not exempted.
var errorLeakExceptions = map[string]bool{
	"internal/admin/admin.go:2668":   true,
	"internal/admin/admin.go:2788":   true,
	"internal/admin/admin.go:3350":   true,
	"internal/admin/admin.go:3367":   true,
	"internal/admin/admin.go:3372":   true,
	"internal/admin/admin.go:3774":   true,
	"internal/admin/admin.go:3786":   true,
	"internal/admin/admin.go:5090":   true,
	"internal/admin/admin.go:5224":   true,
	"internal/admin/admin.go:5358":   true,
	"internal/admin/admin.go:5496":   true,
	"internal/admin/admin.go:7235":   true,
	"internal/admin/admin.go:7489":   true,
	"internal/admin/admin.go:7693":   true,
	"internal/admin/catalog.go:1499": true,
	"internal/admin/catalog.go:1623": true,
	"internal/admin/catalog.go:1630": true,
	"internal/admin/admin.go:8054":   true,
	"internal/admin/admin.go:8110":   true,
	"internal/admin/admin.go:8147":   true,
	"internal/admin/admin.go:8181":   true,
	"internal/admin/admin.go:8222":   true,
}

// adminGoFiles returns the repo root and every non-test .go file tracked
// under internal/admin (absolute paths), via `git ls-files`.
func adminGoFiles(t *testing.T) (string, []string) {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	rootDir := strings.TrimSpace(string(root))

	out, err := exec.Command("git", "-C", rootDir, "ls-files", "-z", "internal/admin").Output()
	if err != nil {
		t.Fatalf("git ls-files internal/admin: %v", err)
	}

	var files []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		p := string(raw)
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		files = append(files, rootDir+"/"+p)
	}
	return rootDir, files
}

// TestNoRawErrorLeaksToClient is the standing guard for the bug class fixed
// in commit 9aff1df: an admin handler must never write a raw err.Error()
// (or an err-derived formatted string) straight into an HTTP response -
// that can leak DB/file/library/network internals to the client. Route
// through writeServerError or writeCorrelatedError instead, which log the
// real error server-side with a correlation ID and return only a generic
// message + that ID.
//
// If this test is failing on a NEW line (not in errorLeakExceptions),
// the fix is to route that call through writeServerError/
// writeCorrelatedError, not to add an exception.
func TestNoRawErrorLeaksToClient(t *testing.T) {
	rootDir, files := adminGoFiles(t)
	if len(files) == 0 {
		t.Fatal("adminGoFiles returned nothing - check the git invocation, not the pattern")
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		rel := strings.TrimPrefix(path, rootDir+"/")
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !errorLeakSinkPattern.MatchString(line) {
				continue
			}
			if !rawErrRefPattern.MatchString(line) {
				continue
			}
			key := rel + ":" + strconv.Itoa(i+1)
			if errorLeakExceptions[key] {
				continue
			}
			t.Errorf("%s: raw error leaked to client: %q - route through writeServerError/writeCorrelatedError instead", key, strings.TrimSpace(line))
		}
	}
}
