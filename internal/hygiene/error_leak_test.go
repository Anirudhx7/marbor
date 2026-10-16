package hygiene

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// errorLeakSinkPattern matches a call believed to write directly to the
// client's http.ResponseWriter with an error-shaped message:
// writeJSONError, http.Error, or an inline json.Encode of an "error" field.
// internal/admin's safe convention is writeServerError/writeCorrelatedError
// (see internal/admin/admin.go), which logs the real error server-side with
// a correlation ID and sends the client only a generic message + that ID -
// never err.Error(). Neither safe function matches this pattern, so
// calling through them never trips this guard.
var errorLeakSinkPattern = regexp.MustCompile(`writeJSONError\(|http\.Error\(|Encode\(map\[string\]string\{"error"`)

// rawErrRefPattern matches a bare `err` identifier reference (word
// boundary) - this project's conventional error variable name. A sink line
// that also references `err` is passing the real error (via err.Error(),
// %v/%s formatting, or similar) to the client instead of a safe message.
var rawErrRefPattern = regexp.MustCompile(`\berr\b`)

// errorLeakExceptions lists known raw-error-to-client leaks that are exempted
// from this guard with a documented reason. Each key is
// "file|enclosing function|exact trimmed source line" and the value is how
// many identical lines that function may contain, so edits elsewhere in the
// file never invalidate an entry but a new copy of an exempted line still
// fails.
//
// The agent action call sites (runtime start/stop/restart/logs, list/delete
// node models, health check, unload model) all dispatch to a *ViaAgent
// helper whose error is one of two things. Either a clean agent-reported
// domain message (e.g. "unsupported: no unload primitive for runtime
// \"vllm\"", or a systemd/process driver failure text) that is deliberately
// meant to surface to the client - locked in by
// TestHandleNodeRuntimeAction_AgentErrorPassthrough,
// TestHandleNodeRuntimeLogs_AgentErrorPassthrough, and
// TestHandleUnloadModel_AgentUnsupportedRuntimeReturnsError - or a
// transport-level failure (dial/TLS/timeout) reaching the agent.
//
// The two cannot be told apart once collapsed into a single Go error at the
// call site, so the fix is applied at the source: every *ViaAgent helper's
// HTTP-client Do() failure branch logs the real error server-side and returns
// a generic sentinel message (runtimeActionViaAgent, runtimeLogsViaAgent,
// listModelsViaAgent, deleteModelViaAgent, healthCheckViaAgent,
// unloadModelViaAgent). So err.Error() at these call sites is always one of
// the safe forms, never raw network/TLS internals.
// TestAgentHelpersDoNotWrapTransportError guards that those helpers keep their
// side of this bargain.
//
// The remaining entries are domain/validation-shaped errors that are
// legitimately meant to reach the client as-is: key-expiry validation,
// routing-strategy validation, node URL/TLS patch validation, settings
// validation, backup-file validation, and the ErrModelPinned/
// ErrUnloadUnsupported sentinels. These must NOT be routed through
// writeCorrelatedError/writeServerError with err.Error() as the message -
// both are documented to "never echo err.Error() to the client", precisely
// so any future caller can trust that routing an error through them is
// always safe even when the error is genuinely sensitive (DB/file/network
// internals). Passing validated-safe text through them anyway would model
// exactly the anti-pattern those functions exist to prevent, so
// writeJSONError - the same mechanism the agent action sites above use - plus
// this documented exception is the correct, consistent fix here too.
//
// Do not add a new entry here without a documented reason - a genuine new
// leak found by this test should be fixed by routing through
// writeServerError/writeCorrelatedError, not exempted.
//
// How the guard works, and where it stops:
//   - A stale entry fails the test: if an exempted line is deleted or its
//     handler or file is renamed, the budget no longer matches what the scan
//     observes, and the entry must be removed or reduced (never raised). This
//     closes drift, not a deliberate swap of one exempted line for a
//     different leaking line under the same key text.
//   - When any file cannot be read or parsed, stale detection is skipped for
//     ALL entries (the observed counts are incomplete), and the report says so.
//   - A count of zero or less is rejected.
//   - The enclosing function is the top-level function declaration, found
//     with the Go parser. Closures count as their enclosing function; code
//     outside any function (a package-level function literal) is attributed
//     to "<pkg-level>". The receiver type is not part of the key, so methods
//     with the same name on different receivers share one budget.
//   - The check is line-textual. A sink call split across several lines, an
//     error held in a variable not named err, an intermediate string variable,
//     or a sink other than writeJSONError/http.Error/Encode is not seen.
//   - Comment lines that mention both a sink and err are matched (false
//     positive); such a line needs an exception entry or rewording.
//   - Renaming an exempted handler legitimately requires editing its key.
var errorLeakExceptions = map[string]int{
	"internal/admin/admin.go|handleNodeRuntimeAction|writeJSONError(w, http.StatusBadGateway, err.Error())":                                          1,
	"internal/admin/admin.go|handleNodeRuntimeLogs|writeJSONError(w, http.StatusBadGateway, err.Error())":                                            1,
	"internal/admin/admin.go|handleUnloadModel|writeJSONError(w, http.StatusBadGateway, err.Error())":                                                1,
	"internal/admin/admin.go|handleUnloadModel|writeJSONError(w, http.StatusConflict, err.Error())":                                                  1,
	"internal/admin/admin.go|handleUnloadModel|writeJSONError(w, http.StatusUnprocessableEntity, err.Error())":                                       1,
	"internal/admin/admin.go|handleNodeModels|writeJSONError(w, http.StatusBadGateway, err.Error())":                                                 1,
	"internal/admin/admin.go|deleteSingleNodeModel|writeJSONError(w, http.StatusBadGateway, err.Error())":                                            1,
	"internal/admin/admin.go|handleNodeHealthCheck|writeJSONError(w, http.StatusBadGateway, err.Error())":                                            1,
	"internal/admin/admin.go|handlePatchNode|writeJSONError(w, status, err.Error())":                                                                 2,
	"internal/admin/admin.go|handleAddKey|writeJSONError(w, http.StatusBadRequest, err.Error())":                                                     1,
	"internal/admin/admin.go|handlePatchKey|writeJSONError(w, http.StatusBadRequest, err.Error())":                                                   1,
	"internal/admin/admin.go|handleSetRoutingStrategy|writeJSONError(w, http.StatusBadRequest, err.Error())":                                         1,
	"internal/admin/admin.go|handleUpdateSettings|writeJSONError(w, http.StatusBadRequest, \"validation failed: \"+err.Error())":                     1,
	"internal/admin/admin.go|handleRestoreBackup|writeJSONError(w, http.StatusUnprocessableEntity, \"backup file failed validation: \"+err.Error())": 1,
	"internal/admin/admin.go|handleUploadBackup|writeJSONError(w, http.StatusUnprocessableEntity, \"not a valid marbor.db backup: \"+err.Error())":   1,
}

// exitStderr returns the captured stderr of a failed command as a
// ready-to-append suffix, or "" when there is none.
func exitStderr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return ": " + strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

// adminGoFiles returns the repo root and every non-test .go file tracked
// under internal/admin (absolute paths), via `git ls-files`.
func adminGoFiles(t *testing.T) (string, []string) {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v%s", err, exitStderr(err))
	}
	rootDir := strings.TrimSpace(string(root))

	out, err := exec.Command("git", "-C", rootDir, "ls-files", "-z", "internal/admin").Output()
	if err != nil {
		t.Fatalf("git ls-files internal/admin: %v%s", err, exitStderr(err))
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

// leakReport is the result of one scan over a set of source files.
type leakReport struct {
	// Observed counts every sink line that references err, per exception key.
	Observed map[string]int
	// Violations are leaks beyond the allowed budget (in file, line order).
	Violations []string
	// Stale are exception entries whose budget exceeds what was observed.
	Stale []string
	// Invalid are exception entries with a count that is not positive.
	Invalid []string
	// ParseErrors name each file that could not be parsed.
	ParseErrors []string
	// ReadErrors name each file that could not be read.
	ReadErrors []string
	// StaleSkipped is set when stale detection did not run because of read or
	// parse errors.
	StaleSkipped bool
}

const staleSkippedNotice = "stale-exception check skipped because of read or parse errors"

// Problems returns every finding in a stable order: read errors (sorted),
// parse errors, invalid entries, violations, stale entries, then the
// skipped-stale notice when it applies.
func (r leakReport) Problems() []string {
	var out []string
	readErrs := append([]string(nil), r.ReadErrors...)
	sort.Strings(readErrs)
	out = append(out, readErrs...)
	out = append(out, r.ParseErrors...)
	out = append(out, r.Invalid...)
	out = append(out, r.Violations...)
	out = append(out, r.Stale...)
	if r.StaleSkipped {
		out = append(out, staleSkippedNotice)
	}
	return out
}

// funcSpan is a top-level function declaration's inclusive line range.
type funcSpan struct {
	name       string
	start, end int
}

const pkgLevelFunc = "<pkg-level>"

// topLevelFuncSpans parses src and returns its function declarations. The
// parser is used only for these line ranges; key text always comes from the
// source lines.
func topLevelFuncSpans(rel string, src []byte) ([]funcSpan, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%s: parse: %w", rel, err)
	}
	var spans []funcSpan
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		// PositionFor with adjusted=false ignores //line directives, so spans
		// always use physical lines, matching strings.Split on the source.
		spans = append(spans, funcSpan{fd.Name.Name, fset.PositionFor(fd.Pos(), false).Line, fset.PositionFor(fd.End(), false).Line})
	}
	return spans, nil
}

func enclosingFunc(spans []funcSpan, line int) string {
	for _, s := range spans {
		if line >= s.start && line <= s.end {
			return s.name
		}
	}
	return pkgLevelFunc
}

// scanErrorLeaks scans one file and returns the violations beyond allowed
// and the observed count per key. It never inspects other files, so stale
// detection is the caller's job (see staleExceptions).
func scanErrorLeaks(rel string, src []byte, allowed map[string]int) (violations []string, observed map[string]int, err error) {
	spans, err := topLevelFuncSpans(rel, src)
	if err != nil {
		return nil, nil, err
	}
	observed = map[string]int{}
	for i, line := range strings.Split(string(src), "\n") {
		if !errorLeakSinkPattern.MatchString(line) || !rawErrRefPattern.MatchString(line) {
			continue
		}
		text := strings.TrimSpace(line)
		fn := enclosingFunc(spans, i+1)
		key := rel + "|" + fn + "|" + text
		observed[key]++
		if observed[key] <= allowed[key] {
			continue
		}
		violations = append(violations, fmt.Sprintf("%s:%d (%s): raw error leaked to client: %q - route through writeServerError/writeCorrelatedError instead", rel, i+1, fn, text))
	}
	return violations, observed, nil
}

// staleExceptions reports every allowed entry whose budget exceeds what the
// scan observed, sorted by key.
func staleExceptions(allowed, observed map[string]int) []string {
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if allowed[k] > 0 && observed[k] < allowed[k] {
			out = append(out, fmt.Sprintf("stale exception %q: observed %d, allowed %d - remove or reduce this exception, never add", k, observed[k], allowed[k]))
		}
	}
	return out
}

// invalidExceptionCounts reports every entry whose count is not positive.
func invalidExceptionCounts(allowed map[string]int) []string {
	var keys []string
	for k, n := range allowed {
		if n <= 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, fmt.Sprintf("exception %q has count %d: counts must be positive", k, allowed[k]))
	}
	return out
}

// runErrorLeakScan scans every file (keyed by forward-slash relative path)
// against allowed and aggregates all findings in one deterministic report.
// readErrors are files the caller could not read; they are reported as-is.
// Stale detection is skipped for all entries when any file failed to read or
// parse, since the observed counts would be incomplete.
func runErrorLeakScan(files map[string][]byte, allowed map[string]int, readErrors []string) leakReport {
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	r := leakReport{Observed: map[string]int{}, ReadErrors: readErrors}
	for _, rel := range rels {
		v, obs, err := scanErrorLeaks(rel, files[rel], allowed)
		if err != nil {
			r.ParseErrors = append(r.ParseErrors, err.Error())
			continue
		}
		r.Violations = append(r.Violations, v...)
		for k, n := range obs {
			r.Observed[k] += n
		}
	}
	r.Invalid = invalidExceptionCounts(allowed)
	if len(r.ParseErrors) == 0 && len(r.ReadErrors) == 0 {
		r.Stale = staleExceptions(allowed, r.Observed)
	} else {
		r.StaleSkipped = true
	}
	return r
}

// readAdminFiles reads every path through read and returns the contents keyed
// by repo-relative path, plus one message per unreadable file (also by
// repo-relative path).
func readAdminFiles(rootDir string, paths []string, read func(string) ([]byte, error)) (map[string][]byte, []string) {
	files := map[string][]byte{}
	var readErrors []string
	for _, path := range paths {
		rel := strings.TrimPrefix(path, rootDir+"/")
		data, err := read(path)
		if err != nil {
			readErrors = append(readErrors, fmt.Sprintf("reading %s: %v", rel, err))
			continue
		}
		files[rel] = data
	}
	return files, readErrors
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
	rootDir, paths := adminGoFiles(t)
	if len(paths) == 0 {
		t.Fatal("adminGoFiles returned nothing - check the git invocation, not the pattern")
	}

	files, readErrors := readAdminFiles(rootDir, paths, os.ReadFile)
	for _, p := range runErrorLeakScan(files, errorLeakExceptions, readErrors).Problems() {
		t.Error(p)
	}
}

// agentHelperFuncNames are the *ViaAgent helpers whose HTTP-client Do()
// failure branch must never wrap the raw transport error - see
// errorLeakExceptions' doc comment for why their callers are allowed to
// pass err.Error() straight to the client.
var agentHelperFuncNames = []string{
	"runtimeActionViaAgent",
	"runtimeLogsViaAgent",
	"listModelsViaAgent",
	"deleteModelViaAgent",
	"healthCheckViaAgent",
	"unloadModelViaAgent",
}

var agentHelperFuncStart = regexp.MustCompile(`^func \(s \*Server\) (\w+)\(`)

// TestAgentHelpersDoNotWrapTransportError guards the half of the fix that
// TestNoRawErrorLeaksToClient cannot see: it only scans for a raw error
// reaching a writeJSONError/http.Error/Encode call, never a *ViaAgent
// helper's own return statement, so a regression inside one of these
// helpers - reverting their HTTP-client Do() failure branch back to
// wrapping the raw error with %w (e.g. fmt.Errorf("agent X failed: %w",
// err), exposing dial/TLS/timeout internals through the error's own
// Error() text) - would slip past it silently: the calling handler's line
// is a pre-approved exception in errorLeakExceptions regardless of what the
// helper now returns.
func TestAgentHelpersDoNotWrapTransportError(t *testing.T) {
	rootDir, files := adminGoFiles(t)
	found := map[string]bool{}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		rel := strings.TrimPrefix(path, rootDir+"/")
		lines := strings.Split(string(data), "\n")
		for i := 0; i < len(lines); i++ {
			m := agentHelperFuncStart.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			name := m[1]
			isTarget := false
			for _, want := range agentHelperFuncNames {
				if name == want {
					isTarget = true
					break
				}
			}
			if !isTarget {
				continue
			}
			found[name] = true
			end := i + 1
			for end < len(lines) && lines[end] != "}" {
				end++
			}
			body := strings.Join(lines[i:end+1], "\n")
			if strings.Contains(body, "%w") {
				t.Errorf("%s: %s wraps an error with %%w - if this is the HTTP-client Do() failure branch, it can leak raw dial/TLS/timeout internals to the client through its callers' err.Error() passthrough; log the real error server-side and return a plain sentinel error instead", rel, name)
			}
		}
	}
	for _, want := range agentHelperFuncNames {
		if !found[want] {
			t.Errorf("expected to find function %s in internal/admin - if it was renamed, update agentHelperFuncNames", want)
		}
	}
}

const (
	leakLine     = "writeJSONError(w, http.StatusBadGateway, err.Error())"
	leakLineAlt  = "writeJSONError(w, http.StatusConflict, err.Error())"
	leakFilePath = "internal/admin/admin.go"
)

func leakKey(file, fn, text string) string { return file + "|" + fn + "|" + text }

// leakDecl renders a function whose signature (name and receiver) is sig.
func leakDecl(sig string, lines ...string) string {
	return "func " + sig + " {\n\t" + strings.Join(lines, "\n\t") + "\n}\n"
}

func leakFunc(name string, lines ...string) string {
	return leakDecl(name+"()", lines...)
}

// lineOf returns the 1-based number of the first line of src whose trimmed
// text is text.
func lineOf(t *testing.T, src []byte, text string) int {
	t.Helper()
	for i, l := range strings.Split(string(src), "\n") {
		if strings.TrimSpace(l) == text {
			return i + 1
		}
	}
	t.Fatalf("line %q not found in fixture", text)
	return 0
}

func leakFile(funcs ...string) []byte {
	return []byte("package admin\n\n" + strings.Join(funcs, "\n"))
}

func scanOne(t *testing.T, file string, src []byte, allowed map[string]int) leakReport {
	t.Helper()
	return runErrorLeakScan(map[string][]byte{file: src}, allowed, nil)
}

func requireClean(t *testing.T, r leakReport) {
	t.Helper()
	if len(r.Problems()) != 0 {
		t.Fatalf("expected a clean report, got: %v", r.Problems())
	}
}

func requireProblem(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("expected a problem containing %q, got none", want)
	}
	joined := strings.Join(got, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Fatalf("expected problems to contain %q, got:\n%s", w, joined)
		}
	}
}

func TestScanErrorLeaks_ShiftedLineStillExempt(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	base := leakFile(leakFunc("handleA", leakLine))
	requireClean(t, scanOne(t, leakFilePath, base, allowed))

	shifted := []byte(strings.Replace(string(base), "package admin\n", "package admin\n"+strings.Repeat("// padding\n", 40), 1))
	requireClean(t, scanOne(t, leakFilePath, shifted, allowed))
}

func TestScanErrorLeaks_ShiftedByNewFunctionAboveStillExempt(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	src := leakFile(leakFunc("helperAbove", "return", "return"), leakFunc("handleA", leakLine))
	requireClean(t, scanOne(t, leakFilePath, src, allowed))
}

func TestScanErrorLeaks_NewLeakInNewFunctionFails(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	src := leakFile(leakFunc("handleA", leakLine), leakFunc("handleNew", leakLine))
	r := scanOne(t, leakFilePath, src, allowed)
	requireProblem(t, r.Violations, "handleNew", "raw error leaked")
	if len(r.Stale) != 0 {
		t.Fatalf("unexpected stale: %v", r.Stale)
	}
}

func TestScanErrorLeaks_NonExemptLeakInsideExemptedFuncFails(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	src := leakFile(leakFunc("handleA", leakLine, leakLineAlt))
	r := scanOne(t, leakFilePath, src, allowed)
	requireProblem(t, r.Violations, "handleA", "StatusConflict")
	if len(r.Stale) != 0 {
		t.Fatalf("unexpected stale: %v", r.Stale)
	}
}

func TestScanErrorLeaks_SameTextInOtherFileFails(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	src := leakFile(leakFunc("handleA", leakLine))
	r := runErrorLeakScan(map[string][]byte{
		leakFilePath:             src,
		"internal/admin/more.go": src,
	}, allowed, nil)
	requireProblem(t, r.Violations, "internal/admin/more.go")
	if len(r.Stale) != 0 {
		t.Fatalf("unexpected stale: %v", r.Stale)
	}
}

func TestScanErrorLeaks_ExtraIdenticalLeakFails(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 2}
	src := leakFile(leakFunc("handleA", leakLine, leakLine, leakLine))
	r := scanOne(t, leakFilePath, src, allowed)
	if len(r.Violations) != 1 {
		t.Fatalf("expected exactly one violation (third copy), got %v", r.Violations)
	}
}

func TestScanErrorLeaks_ChangedLineTextFails(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	src := leakFile(leakFunc("handleA", "writeJSONError(w, http.StatusTeapot, err.Error())"))
	r := scanOne(t, leakFilePath, src, allowed)
	requireProblem(t, r.Violations, "StatusTeapot")
	requireProblem(t, r.Stale, leakKey(leakFilePath, "handleA", leakLine))
}

func TestScanErrorLeaks_StaleBudgetFails(t *testing.T) {
	t.Parallel()
	key := leakKey(leakFilePath, "handleA", leakLine)
	cases := []struct {
		name      string
		body      []string
		budget    int
		wantParts []string
	}{
		{"nothing observed", []string{"return"}, 1, []string{key, "observed 0", "allowed 1", "never add"}},
		{"partial budget", []string{leakLine}, 2, []string{key, "observed 1", "allowed 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scanOne(t, leakFilePath, leakFile(leakFunc("handleA", tc.body...)), map[string]int{key: tc.budget})
			requireProblem(t, r.Stale, tc.wantParts...)
			if len(r.Violations) != 0 {
				t.Fatalf("unexpected violations: %v", r.Violations)
			}
		})
	}
}

func TestScanErrorLeaks_StaleAndNewReportedTogetherAndSorted(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{
		leakKey(leakFilePath, "zeta", leakLine):  1,
		leakKey(leakFilePath, "alpha", leakLine): 1,
	}
	r := scanOne(t, leakFilePath, leakFile(leakFunc("other", leakLine)), allowed)
	if len(r.Stale) != 2 || !sort.StringsAreSorted(r.Stale) {
		t.Fatalf("expected two sorted stale entries, got %v", r.Stale)
	}
	if len(r.Violations) != 1 {
		t.Fatalf("expected the new leak to be reported in the same pass, got %v", r.Violations)
	}
}

func TestScanErrorLeaks_CRLFSourceKeysIdentical(t *testing.T) {
	t.Parallel()
	lf := leakFile(leakFunc("handleA", leakLine, leakLine))
	crlf := []byte(strings.ReplaceAll(string(lf), "\n", "\r\n"))
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 2}
	a := scanOne(t, leakFilePath, lf, allowed)
	b := scanOne(t, leakFilePath, crlf, allowed)
	requireClean(t, a)
	requireClean(t, b)
	if !reflect.DeepEqual(a.Observed, b.Observed) {
		t.Fatalf("observed differs between LF and CRLF: %v vs %v", a.Observed, b.Observed)
	}
	if b.Observed[leakKey(leakFilePath, "handleA", leakLine)] != 2 {
		t.Fatalf("CRLF observed wrong: %v", b.Observed)
	}
}

func TestScanErrorLeaks_ParseErrorNamesFile(t *testing.T) {
	t.Parallel()
	r := scanOne(t, "internal/admin/broken.go", []byte("package admin\nfunc {{{\n"), nil)
	if len(r.ParseErrors) != 1 || !strings.HasPrefix(r.ParseErrors[0], "internal/admin/broken.go: parse: ") {
		t.Fatalf("expected one parse error prefixed with the file name, got %v", r.ParseErrors)
	}
}

func TestScanErrorLeaks_CountNonPositiveRejected(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{
		leakKey(leakFilePath, "a", leakLine): 0,
		leakKey(leakFilePath, "b", leakLine): -1,
	}
	r := scanOne(t, leakFilePath, leakFile(leakFunc("c", "return")), allowed)
	if len(r.Invalid) != 2 {
		t.Fatalf("expected two invalid-count entries, got %v", r.Invalid)
	}
	requireProblem(t, r.Invalid, "positive")
}

func TestScanErrorLeaks_AdminGoMissingGivesStaleAndNewLeak(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 1}
	src := leakFile(leakFunc("handleA", leakLine))
	r := scanOne(t, "internal/admin/renamed.go", src, allowed)
	requireProblem(t, r.Stale, leakFilePath)
	requireProblem(t, r.Violations, "internal/admin/renamed.go")
}

func TestScanErrorLeaks_FuncAttribution(t *testing.T) {
	t.Parallel()
	src := []byte(`package admin

var pkgLevel = func() {
	writeJSONError(w, 400, err.Error())
}

func Top() {
	f := func() {
		writeJSONError(w, 401, err.Error())
	}
	_ = f
}

func (s *Server) Method() {
	writeJSONError(w, 402, err.Error())
}

func (b *Box[T]) Generic() {
	writeJSONError(w, 403, err.Error())
}
`)
	r := scanOne(t, "p.go", src, nil)
	want := map[string]int{
		"p.go|<pkg-level>|writeJSONError(w, 400, err.Error())": 1,
		"p.go|Top|writeJSONError(w, 401, err.Error())":         1,
		"p.go|Method|writeJSONError(w, 402, err.Error())":      1,
		"p.go|Generic|writeJSONError(w, 403, err.Error())":     1,
	}
	if !reflect.DeepEqual(r.Observed, want) {
		t.Fatalf("attribution mismatch:\n got %v\nwant %v", r.Observed, want)
	}
}

func TestScanErrorLeaks_PhysicalLinesIgnoreLineDirective(t *testing.T) {
	t.Parallel()
	src := []byte("package admin\n\n//line other.go:100\nfunc handleA() {\n\twriteJSONError(w, 400, err.Error())\n}\n")
	r := scanOne(t, "p.go", src, nil)
	want := map[string]int{"p.go|handleA|writeJSONError(w, 400, err.Error())": 1}
	if !reflect.DeepEqual(r.Observed, want) {
		t.Fatalf("attribution skewed by //line directive:\n got %v\nwant %v", r.Observed, want)
	}
}

func TestScanErrorLeaks_ReadErrorSkipsStale(t *testing.T) {
	t.Parallel()
	key := leakKey(leakFilePath, "handleA", leakLine)
	files := map[string][]byte{"internal/admin/other.go": leakFile(leakFunc("handleNew", leakLine))}
	r := runErrorLeakScan(files, map[string]int{key: 1}, []string{"reading internal/admin/admin.go: boom"})
	if len(r.Stale) != 0 {
		t.Fatalf("stale must be skipped after a read error, got %v", r.Stale)
	}
	requireProblem(t, r.Problems(), "reading internal/admin/admin.go: boom", "handleNew", staleSkippedNotice)

	// Control: without the read error the same input reports the stale entry
	// and no skipped notice.
	c := runErrorLeakScan(files, map[string]int{key: 1}, nil)
	requireProblem(t, c.Stale, key)
	if c.StaleSkipped || strings.Contains(strings.Join(c.Problems(), "\n"), staleSkippedNotice) {
		t.Fatalf("skipped notice must not appear when stale ran, got %v", c.Problems())
	}
}

func TestScanErrorLeaks_ParseErrorSkipsStale(t *testing.T) {
	t.Parallel()
	key := leakKey(leakFilePath, "handleA", leakLine)
	r := runErrorLeakScan(map[string][]byte{
		"internal/admin/broken.go": []byte("package admin\nfunc {{{\n"),
		"internal/admin/other.go":  leakFile(leakFunc("handleNew", leakLine)),
	}, map[string]int{key: 1}, nil)
	if len(r.Stale) != 0 {
		t.Fatalf("stale must be skipped after a parse error, got %v", r.Stale)
	}
	requireProblem(t, r.ParseErrors, "internal/admin/broken.go")
	requireProblem(t, r.Violations, "internal/admin/other.go", "handleNew")
	requireProblem(t, r.Problems(), staleSkippedNotice)
}

func TestReadAdminFiles_ReadFailureIsReportedByRelativePath(t *testing.T) {
	t.Parallel()
	root := "/repo"
	paths := []string{root + "/internal/admin/a.go", root + "/internal/admin/b.go"}
	read := func(p string) ([]byte, error) {
		if p == paths[1] {
			return nil, errors.New("boom")
		}
		return []byte("package admin\n"), nil
	}
	files, readErrors := readAdminFiles(root, paths, read)
	if _, ok := files["internal/admin/a.go"]; !ok || len(files) != 1 {
		t.Fatalf("expected only a.go to be read, got %v", files)
	}
	want := []string{"reading internal/admin/b.go: boom"}
	if !reflect.DeepEqual(readErrors, want) {
		t.Fatalf("read errors: got %v want %v", readErrors, want)
	}
	// Wired through the scan, the failure surfaces and stale is skipped.
	r := runErrorLeakScan(files, map[string]int{leakKey(leakFilePath, "h", leakLine): 1}, readErrors)
	requireProblem(t, r.Problems(), want[0], staleSkippedNotice)
}

func TestLeakReport_ProblemsOrder(t *testing.T) {
	t.Parallel()
	r := leakReport{
		ReadErrors:   []string{"read-b", "read-a"},
		ParseErrors:  []string{"parse-1"},
		Invalid:      []string{"invalid-1"},
		Violations:   []string{"violation-1"},
		Stale:        []string{"stale-1"},
		StaleSkipped: true,
	}
	want := []string{"read-a", "read-b", "parse-1", "invalid-1", "violation-1", "stale-1", staleSkippedNotice}
	if got := r.Problems(); !reflect.DeepEqual(got, want) {
		t.Fatalf("problem order:\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(r.ReadErrors, []string{"read-b", "read-a"}) {
		t.Fatalf("Problems must not reorder the report's own slice, got %v", r.ReadErrors)
	}
}

func TestExitStderr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"stderr is trimmed", &exec.ExitError{Stderr: []byte("  fatal: boom\n")}, ": fatal: boom"},
		{"wrapped exit error", fmt.Errorf("wrap: %w", &exec.ExitError{Stderr: []byte("bad\n")}), ": bad"},
		{"empty stderr", &exec.ExitError{}, ""},
		{"whitespace-only stderr", &exec.ExitError{Stderr: []byte(" \n\t")}, ""},
		{"not an exit error", errors.New("plain"), ""},
		{"nil error", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitStderr(tc.err); got != tc.want {
				t.Fatalf("exitStderr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanErrorLeaks_ParseErrorIsWrapped(t *testing.T) {
	t.Parallel()
	_, err := topLevelFuncSpans("x.go", []byte("package admin\nfunc {{{\n"))
	var list scanner.ErrorList
	if !errors.As(err, &list) {
		t.Fatalf("expected the parser's error to be wrapped with %%w, got %T: %v", err, err)
	}
}

func TestScanErrorLeaks_ViolationsOrderedByFileThenLine(t *testing.T) {
	t.Parallel()
	src := leakFile(leakFunc("h", leakLine, leakLineAlt))
	r := runErrorLeakScan(map[string][]byte{"b.go": src, "a.go": src}, nil, nil)
	if len(r.Violations) != 4 {
		t.Fatalf("expected 4 violations, got %v", r.Violations)
	}
	l1, l2 := lineOf(t, src, leakLine), lineOf(t, src, leakLineAlt)
	prefixes := []string{
		fmt.Sprintf("a.go:%d", l1), fmt.Sprintf("a.go:%d", l2),
		fmt.Sprintf("b.go:%d", l1), fmt.Sprintf("b.go:%d", l2),
	}
	for i, p := range prefixes {
		if !strings.HasPrefix(r.Violations[i], p+" ") {
			t.Fatalf("violation %d should start with %q, got %q", i, p, r.Violations[i])
		}
	}
}

func TestScanErrorLeaks_ZeroCountWithObservedLeakIsViolationAndInvalid(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{leakKey(leakFilePath, "handleA", leakLine): 0}
	r := scanOne(t, leakFilePath, leakFile(leakFunc("handleA", leakLine)), allowed)
	requireProblem(t, r.Violations, "handleA", "raw error leaked")
	requireProblem(t, r.Invalid, "positive")
	if len(r.Stale) != 0 {
		t.Fatalf("a non-positive count must not also be reported stale, got %v", r.Stale)
	}
}

func TestScanErrorLeaks_SinkDetection(t *testing.T) {
	t.Parallel()
	const encodeLine = `json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})`
	const httpErrLine = "http.Error(w, err.Error(), 500)"
	const commentLine = "// writeJSONError(w, 400, err.Error())"
	const encodeNoErr = `json.NewEncoder(w).Encode(map[string]string{"error": "bad"})`
	const wordBoundaryLine = `writeJSONError(w, 400, errs.Message+myerr+Err)`
	const twoSinkLine = "writeJSONError(w, 400, err.Error()); http.Error(w, err.Error(), 500)"
	cases := []struct {
		name string
		body []string
		want map[string]int
	}{
		{"sink without err", []string{`writeJSONError(w, 400, "bad request")`}, map[string]int{}},
		{"err without sink", []string{"x := err"}, map[string]int{}},
		{"safe helper with err", []string{`writeServerError(w, r, "failed", err)`}, map[string]int{}},
		{"Encode error field without err", []string{encodeNoErr}, map[string]int{}},
		{"http.Error without err", []string{`http.Error(w, "bad", 500)`}, map[string]int{}},
		{"err only as part of longer identifiers", []string{wordBoundaryLine}, map[string]int{}},
		{"two sinks on one line count once", []string{twoSinkLine}, map[string]int{leakKey("p.go", "h", twoSinkLine): 1}},
		{"http.Error", []string{httpErrLine}, map[string]int{leakKey("p.go", "h", httpErrLine): 1}},
		{"inline Encode error field", []string{encodeLine}, map[string]int{leakKey("p.go", "h", encodeLine): 1}},
		// Known limits, pinned so a change in behavior is a conscious decision.
		{"multi-line sink is not seen", []string{"writeJSONError(w,", "400, err.Error())"}, map[string]int{}},
		{"non-err variable is not seen", []string{"writeJSONError(w, 400, e.Error())"}, map[string]int{}},
		{"comment line is matched (false positive)", []string{commentLine}, map[string]int{leakKey("p.go", "h", commentLine): 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scanOne(t, "p.go", leakFile(leakFunc("h", tc.body...)), nil)
			if !reflect.DeepEqual(r.Observed, tc.want) {
				t.Fatalf("observed mismatch:\n got %v\nwant %v", r.Observed, tc.want)
			}
		})
	}
}

func TestScanErrorLeaks_SameMethodNameOnDifferentReceiversSharesBudget(t *testing.T) {
	t.Parallel()
	src := leakFile(leakDecl("(a *A) Same()", leakLine), leakDecl("(b *B) Same()", leakLine))
	key := leakKey("p.go", "Same", leakLine)
	requireClean(t, scanOne(t, "p.go", src, map[string]int{key: 2}))

	r := scanOne(t, "p.go", src, map[string]int{key: 1})
	if !reflect.DeepEqual(r.Observed, map[string]int{key: 2}) {
		t.Fatalf("both receivers must share one key, got %v", r.Observed)
	}
	if len(r.Violations) != 1 || !strings.Contains(r.Violations[0], "(Same)") {
		t.Fatalf("expected exactly one violation for the second copy, got %v", r.Violations)
	}
}

func TestScanErrorLeaks_SinkOutsideFunctionsIsPkgLevel(t *testing.T) {
	t.Parallel()
	src := leakFile("func A() {}\n", "var x = writeJSONError(w, 1, err.Error())\n", "// writeJSONError(w, 2, err.Error())\nfunc B() {}\n")
	r := scanOne(t, "p.go", src, nil)
	want := map[string]int{
		"p.go|<pkg-level>|var x = writeJSONError(w, 1, err.Error())": 1,
		"p.go|<pkg-level>|// writeJSONError(w, 2, err.Error())":      1,
	}
	if !reflect.DeepEqual(r.Observed, want) {
		t.Fatalf("pkg-level/doc-comment attribution mismatch:\n got %v\nwant %v", r.Observed, want)
	}
}
