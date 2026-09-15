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
// leaks that are exempted from this guard with a documented reason.
//
// The first six entries are all "agent action" call sites (runtime
// start/stop/restart/logs, list/delete node models, health check, unload
// model): each dispatches to a *ViaAgent helper whose error can be either a
// clean agent-reported domain message (e.g. "unsupported: no unload
// primitive for runtime \"vllm\"", or a systemd/process driver failure
// text) that is deliberately meant to surface to the client - locked in by
// TestHandleNodeRuntimeAction_AgentErrorPassthrough,
// TestHandleNodeRuntimeLogs_AgentErrorPassthrough, and
// TestHandleUnloadModel_AgentUnsupportedRuntimeReturnsError - or a
// transport-level failure (dial/TLS/timeout) reaching the agent. The two
// can't be told apart once collapsed into a single Go `error` at this call
// site, so the fix is applied at the source instead: every *ViaAgent
// helper's HTTP-client Do() failure branch already logs the real error
// server-side and returns a generic sentinel message (see
// runtimeActionViaAgent, runtimeLogsViaAgent, listModelsViaAgent,
// deleteModelViaAgent, healthCheckViaAgent, unloadModelViaAgent) - so
// err.Error() at these six call sites is always one of those two safe
// forms, never raw network/TLS internals. TestAgentHelpersDoNotWrapTransportError
// guards that the six helpers keep their side of this bargain.
//
// The remaining ten entries are domain/validation-shaped errors that are
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
// writeJSONError - the same mechanism the six agent-action sites above
// use - plus this documented exception is the correct, consistent fix here
// too.
//
// Do not add a new entry here without a documented reason - a genuine new
// leak found by this test should be fixed by routing through
// writeServerError/writeCorrelatedError, not exempted.
var errorLeakExceptions = map[string]bool{
	"internal/admin/admin.go:2675": true,
	"internal/admin/admin.go:2804": true,
	"internal/admin/admin.go:3366": true,
	"internal/admin/admin.go:7252": true,
	"internal/admin/admin.go:7510": true,
	"internal/admin/admin.go:7718": true,
	"internal/admin/admin.go:3382": true,
	"internal/admin/admin.go:3386": true,
	"internal/admin/admin.go:3788": true,
	"internal/admin/admin.go:3800": true,
	"internal/admin/admin.go:5104": true,
	"internal/admin/admin.go:5238": true,
	"internal/admin/admin.go:5372": true,
	"internal/admin/admin.go:5510": true,
	"internal/admin/admin.go:8137": true,
	"internal/admin/admin.go:8249": true,
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
// helper's own return statement, so a regression inside one of these six
// functions - reverting their HTTP-client Do() failure branch back to
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
