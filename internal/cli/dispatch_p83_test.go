package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dispatch_p83_test.go is the permanent regression net for P83 ("ollama-mesh
// whoami status" and "ollama-mesh whoami dead" both silently ignored the
// extra argument and printed normal whoami output). It walks the live
// registry - the real cli.Run entrypoint, now fully wired to the registry-
// driven dispatcher (see the P83+ CLI hardening plan, migration steps 6-8) -
// and asserts that EVERY runnable leaf command rejects one trailing garbage
// positional argument with ExitUserError, and that it does so before ever
// making a real HTTP request to the Admin API. Because this walks the
// registry generically rather than hand-listing commands, any leaf added in
// the future is covered automatically.

// leafCommands returns every runnable leaf in the tree (Run != nil, no Sub) -
// pure groups (Run == nil) and bare-executable groups that also have
// subcommands (models, nodes) are excluded: their "extra argument" behavior
// is "unknown action", a different (and already covered) code path, not the
// P83 arity bug this test targets.
func leafCommands(c *Command) []*Command {
	var leaves []*Command
	if c.Run != nil && len(c.Sub) == 0 {
		leaves = append(leaves, c)
	}
	for _, s := range c.Sub {
		leaves = append(leaves, leafCommands(s)...)
	}
	return leaves
}

// minimalValidArgsWithGarbage builds a valid invocation for leaf (one
// placeholder positional per required ArgSpec, one placeholder value per
// required FlagSpec) plus one trailing garbage positional - the exact shape
// of the original P83 report ("whoami dead", "whoami status").
func minimalValidArgsWithGarbage(leaf *Command) []string {
	path := strings.Fields(leaf.Path())
	args := append([]string{}, path[1:]...) // drop the leading "ollama-mesh" token

	for _, a := range leaf.Args {
		if a.Variadic {
			continue
		}
		args = append(args, "placeholder-value")
	}

	for _, f := range leaf.Flags {
		if !f.Required {
			continue
		}
		switch f.Kind {
		case FlagString:
			args = append(args, "--"+f.Name+"=placeholder-value")
		case FlagBool:
			args = append(args, "--"+f.Name)
		case FlagInt:
			args = append(args, "--"+f.Name+"=1")
		}
	}

	args = append(args, "trailing-garbage-argument")
	return args
}

func TestP83_AllLeavesRejectTrailingGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should never be contacted for a trailing-garbage invocation, but got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	leaves := leafCommands(root())
	if len(leaves) == 0 {
		t.Fatal("leafCommands(root()) returned no leaves - registry walk is broken")
	}

	for _, leaf := range leaves {
		leaf := leaf
		t.Run(leaf.Path(), func(t *testing.T) {
			args := minimalValidArgsWithGarbage(leaf)
			args = append(args, "--server", srv.URL, "--username", "admin", "--password", "admin", "--json")

			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)

			if code != ExitUserError {
				t.Fatalf("expected ExitUserError (%d) for trailing garbage on %q, got %d\nargs=%v\nstdout=%q\nstderr=%q",
					ExitUserError, leaf.Path(), code, args, stdout.String(), stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "usage: ") {
				t.Fatalf("expected a bare 'usage: ...' arity line on stderr for %q, got stderr=%q", leaf.Path(), stderr.String())
			}
		})
	}
}

// TestIsZeroFlagValue_StringLiteralsNotTreatedAsUnset is the regression test
// for Fix 1 of the P83+ CLI hardening code review: isZeroFlagValue used to
// treat a FlagString flag's literal value "0" or "false" as indistinguishable
// from "not supplied", which wrongly rejected valid values like a container
// or PID identifier literally named "0" with "error: --identifier is
// required". "node control accept" (registry_tree.go) is the real command
// whose two Required flags, --driver and --identifier, are both FlagString -
// exercised here via dispatch() directly (not dispatchAndRun/Run) so the
// test only asserts on validation, never reaching the point of an actual
// Admin API call.
func TestIsZeroFlagValue_StringLiteralsNotTreatedAsUnset(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
	}{
		{"identifier literally \"0\"", "0"},
		{"identifier literally \"false\"", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"node", "control", "accept", "somenode", "--driver", "systemd", "--identifier", tc.identifier}

			var stdout, stderr bytes.Buffer
			result := dispatch(root(), args, &stdout, &stderr)

			if result.handled {
				t.Fatalf("expected validation to pass (handled=false, ready to call Run) for --identifier=%q, got handled=true, code=%d, stderr=%q",
					tc.identifier, result.code, stderr.String())
			}
			if strings.Contains(stderr.String(), "required") {
				t.Fatalf("--identifier=%q was wrongly rejected as missing: stderr=%q", tc.identifier, stderr.String())
			}
			if result.matched == nil || result.matched.Path() != "ollama-mesh node control accept" {
				t.Fatalf("expected matched command %q, got %v", "ollama-mesh node control accept", result.matched)
			}
			if got := result.ctx.String("identifier"); got != tc.identifier {
				t.Fatalf("expected ctx.String(\"identifier\") == %q, got %q", tc.identifier, got)
			}
		})
	}
}

// TestP83_OriginalReport pins the exact two invocations from the original
// bug report byte-for-byte: both used to print normal whoami output as if
// the extra argument did not exist.
func TestP83_OriginalReport(t *testing.T) {
	for _, extra := range []string{"dead", "status"} {
		extra := extra
		t.Run(extra, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run([]string{"whoami", extra}, &stdout, &stderr)
			if code != ExitUserError {
				t.Fatalf("whoami %s: expected ExitUserError, got %d (stdout=%q stderr=%q)", extra, code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("whoami %s: expected no stdout output, got %q", extra, stdout.String())
			}
		})
	}
}
