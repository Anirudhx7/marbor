package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// aliasFakeServer is an in-memory stand-in for the model-aliases Admin API
// (never the real config or a real server).
type aliasFakeServer struct {
	mu      sync.Mutex
	aliases map[string]string
	calls   []string
}

func newAliasFakeServer(t *testing.T) (*httptest.Server, *aliasFakeServer) {
	t.Helper()
	f := &aliasFakeServer{aliases: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/admin/model-aliases" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			rows := []ModelAlias{}
			for a, tgt := range f.aliases {
				rows = append(rows, ModelAlias{Alias: a, Target: tgt, TargetAvailable: true, TargetStatus: "loaded", ShadowsModel: a == "mistral:7b", InventoryChecked: true})
			}
			json.NewEncoder(w).Encode(rows)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			var body map[string]string
			json.Unmarshal(b, &body)
			if body["alias"] == body["target"] {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"routing.model_aliases: \"x\" cannot point to itself"}`))
				return
			}
			f.aliases[body["alias"]] = body["target"]
			json.NewEncoder(w).Encode(ModelAlias{Alias: body["alias"], Target: body["target"], ShadowsModel: body["alias"] == "mistral:7b", InventoryChecked: true, TargetAvailable: true, TargetStatus: "loaded"})
		case http.MethodDelete:
			a := r.URL.Query().Get("alias")
			if _, ok := f.aliases[a]; !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"model alias not found"}`))
				return
			}
			delete(f.aliases, a)
			w.Write([]byte(`{"status":"removed"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestModelsAlias_ListSetRemove(t *testing.T) {
	srv, f := newAliasFakeServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"models", "alias", "list", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("list exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no model aliases configured") {
		t.Fatalf("empty list output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"models", "alias", "set", "gpt-4", "llama3.2:8b", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("set exit %d: %s", code, stderr.String())
	}
	if f.aliases["gpt-4"] != "llama3.2:8b" {
		t.Fatalf("server state = %v", f.aliases)
	}
	if !strings.Contains(stdout.String(), `"gpt-4" -> "llama3.2:8b"`) {
		t.Fatalf("set output = %q", stdout.String())
	}

	// A shadowing alias prints a warning on set and on list.
	stdout.Reset()
	stderr.Reset()
	Run([]string{"models", "alias", "set", "mistral:7b", "llama3.2:8b", "--server", srv.URL}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("expected shadow warning on set, stderr = %q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"models", "alias", "list", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(stdout.String(), "gpt-4") || !strings.Contains(stdout.String(), "llama3.2:8b") || !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("list stdout = %q stderr = %q", stdout.String(), stderr.String())
	}

	// JSON output.
	stdout.Reset()
	if code := Run([]string{"models", "alias", "list", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("list --json exit %d", code)
	}
	var rows []ModelAlias
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("json rows = %v (%v) from %q", rows, err, stdout.String())
	}

	// Server-side validation errors surface as a user error.
	stderr.Reset()
	if code := Run([]string{"models", "alias", "set", "x", "x", "--server", srv.URL}, &stdout, &stderr); code != ExitUserError {
		t.Fatalf("invalid set exit = %d, want %d (stderr %q)", code, ExitUserError, stderr.String())
	}

	stdout.Reset()
	if code := Run([]string{"models", "alias", "remove", "gpt-4", "--yes", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("remove exit %d: %s", code, stderr.String())
	}
	if _, ok := f.aliases["gpt-4"]; ok {
		t.Fatal("alias not removed on the server")
	}
	if !strings.Contains(stdout.String(), `model alias "gpt-4" removed`) {
		t.Fatalf("remove output = %q", stdout.String())
	}
}

func TestModelsAliasRemove_RequiresConfirm(t *testing.T) {
	srv, f := newAliasFakeServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	f.aliases["gpt-4"] = "llama3.2:8b"

	origTTY, origReader := stdinIsTTY, stdinReader
	defer func() { stdinIsTTY, stdinReader = origTTY, origReader }()

	// No TTY and no --yes: refuse without contacting the server.
	stdinIsTTY = func() bool { return false }
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"models", "alias", "remove", "gpt-4", "--server", srv.URL}, &stdout, &stderr); code == ExitOK {
		t.Fatal("remove without --yes on a non-TTY must fail")
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("stderr = %q, want a hint about --yes", stderr.String())
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 || f.aliases["gpt-4"] == "" {
		t.Fatalf("server contacted (%d calls) or alias removed without confirmation", calls)
	}

	// TTY, answer "n": aborted, nothing removed.
	stdinIsTTY = func() bool { return true }
	stdinReader = strings.NewReader("n\n")
	stderr.Reset()
	if code := Run([]string{"models", "alias", "remove", "gpt-4", "--server", srv.URL}, &stdout, &stderr); code == ExitOK {
		t.Fatal("declined confirmation must not succeed")
	}
	if !strings.Contains(stderr.String(), "will stop reaching its target") {
		t.Fatalf("prompt = %q, want the specific consequence", stderr.String())
	}
	if f.aliases["gpt-4"] == "" {
		t.Fatal("alias removed despite a declined confirmation")
	}

	// TTY, answer "y": removed.
	stdinReader = strings.NewReader("y\n")
	if code := Run([]string{"models", "alias", "remove", "gpt-4", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("confirmed remove exit %d: %s", code, stderr.String())
	}
	if _, ok := f.aliases["gpt-4"]; ok {
		t.Fatal("alias not removed after confirmation")
	}
}

// TestModelsAlias_NotParsedAsNodePositional guards the dispatch: "alias" is a
// distinct subcommand of models and must never be treated as a node name for
// the per-node subcommands.
func TestModelsAlias_NotParsedAsNodePositional(t *testing.T) {
	srv, f := newAliasFakeServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"models", "alias", "list", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || f.calls[0] != "GET /admin/model-aliases" {
		t.Fatalf("calls = %v, want exactly GET /admin/model-aliases", f.calls)
	}
}
