package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const suggestionListBody = `{
  "suggestions": [
    {"fingerprint":"aaaaaaaaaaaaaaaa","launcher":"vllm-mp","runtime":"vllm","state":"complete","reason":"",
     "head":"gpu-a","members":["gpu-a","gpu-b"],"evidence":[],"missing":[],"declared":[],"confirmable":true,"dismissed":false},
    {"fingerprint":"bbbbbbbbbbbbbbbb","launcher":"llamacpp-rpc","runtime":"llamacpp","state":"incomplete",
     "reason":"1 RPC servers not registered as nodes","head":"gpu-c","members":["gpu-c"],"evidence":[],
     "missing":["1 RPC servers not registered as nodes"],"declared":[],"confirmable":false,"dismissed":false}
  ],
  "coverage": [
    {"node":"gpu-a","host":"h1","state":"reporting","detected":true,"detail":"multi-host launch detected"},
    {"node":"gpu-d","host":"h4","state":"agent_update_needed","detected":false,"detail":"agent update needed to detect multi-host launches"}
  ],
  "dismissedCount": 2
}`

type suggestionServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string // "METHOD request-uri"
}

func newSuggestionServer(t *testing.T, confirmStatus int, confirmBody string) *suggestionServer {
	t.Helper()
	ss := &suggestionServer{}
	ss.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ss.mu.Lock()
		ss.calls = append(ss.calls, r.Method+" "+r.URL.RequestURI())
		ss.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(suggestionListBody))
		case strings.HasSuffix(r.URL.Path, "/confirm"):
			w.WriteHeader(confirmStatus)
			w.Write([]byte(confirmBody))
		default:
			w.Write([]byte(`{"fingerprint":"x","dismissed":true}`))
		}
	}))
	t.Cleanup(ss.srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, ss.srv.URL, "tok")
	return ss
}

func (ss *suggestionServer) wrote() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var out []string
	for _, c := range ss.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

const confirmOKBody = `{"fingerprint":"aaaaaaaaaaaaaaaa","head":"gpu-a","members":["gpu-a","gpu-b"],
 "roles":[{"node":"gpu-a","role":"head","head":"gpu-a"},{"node":"gpu-b","role":"worker","head":"gpu-a"}]}`

func withTTY(t *testing.T, tty bool, input string) {
	t.Helper()
	origTTY, origReader := stdinIsTTY, stdinReader
	stdinIsTTY = func() bool { return tty }
	stdinReader = strings.NewReader(input)
	t.Cleanup(func() { stdinIsTTY, stdinReader = origTTY, origReader })
}

func TestRun_NodesSuggestions_Table(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "--server", ss.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"ID", "STATE", "aaaaaaaaaaaaaaaa", "gpu-a,gpu-b", "ready: marbor nodes suggestions confirm aaaaaaaaaaaaaaaa",
		"1 RPC servers not registered as nodes", "2 dismissed (use --all to show them)", "Not reporting multi-host launch details:", "gpu-d: agent update needed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gpu-a: multi-host launch detected") {
		t.Errorf("reporting nodes must not be repeated in the coverage list:\n%s", out)
	}
	if got := ss.calls[0]; got != "GET /admin/v1/replica-suggestions" {
		t.Errorf("request = %q", got)
	}
}

func TestRun_NodesSuggestions_AllAndJSON(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "--all", "--json", "--server", ss.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := ss.calls[0]; got != "GET /admin/v1/replica-suggestions?includeDismissed=true" {
		t.Errorf("request = %q", got)
	}
	var parsed ReplicaSuggestionsResponse
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout.String())
	}
	if len(parsed.Suggestions) != 2 || parsed.DismissedCount != 2 || len(parsed.Coverage) != 2 {
		t.Errorf("parsed = %+v", parsed)
	}
}

func TestRun_NodesSuggestionsConfirm_RefusesWithoutYesOrTTY(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	withTTY(t, false, "")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--server", ss.srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUserError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "without --yes") {
		t.Errorf("stderr = %s", stderr.String())
	}
	if w := ss.wrote(); len(w) != 0 {
		t.Errorf("refused confirm still wrote: %v", w)
	}
}

func TestRun_NodesSuggestionsConfirm_PromptNamesMembersAndSaysItReverts(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	withTTY(t, true, "y\n")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--server", ss.srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	prompt := stderr.String()
	for _, want := range []string{`head "gpu-a"`, "gpu-b", "Nothing is restarted", "in-flight requests are unaffected", "--replica-members"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "cannot be undone") {
		t.Errorf("this change is reversible, the prompt must not say otherwise:\n%s", prompt)
	}
	if w := ss.wrote(); len(w) != 1 || w[0] != "POST /admin/v1/replica-suggestions/aaaaaaaaaaaaaaaa/confirm" {
		t.Errorf("writes = %v", w)
	}
	if !strings.Contains(stdout.String(), "gpu-b: worker") {
		t.Errorf("stdout = %s", stdout.String())
	}
}

func TestRun_NodesSuggestionsConfirm_AnswerNoAborts(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	withTTY(t, true, "n\n")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--server", ss.srv.URL}, &stdout, &stderr); code != ExitUserError {
		t.Fatalf("exit %d", code)
	}
	if w := ss.wrote(); len(w) != 0 {
		t.Errorf("aborted confirm wrote: %v", w)
	}
}

func TestRun_NodesSuggestionsConfirm_YesAndJSON(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	withTTY(t, false, "")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--yes", "--json", "--server", ss.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out["head"] != "gpu-a" {
		t.Errorf("stdout = %s (%v)", stdout.String(), err)
	}
	if strings.Contains(stderr.String(), "Proceed?") {
		t.Error("--yes must not prompt")
	}
}

func TestRun_NodesSuggestionsConfirm_NotConfirmableNeverPosts(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "bbbbbbbbbbbbbbbb", "--yes", "--server", ss.srv.URL}, &stdout, &stderr)
	if code != ExitUserError || !strings.Contains(stderr.String(), "cannot be confirmed") || !strings.Contains(stderr.String(), "1 RPC servers not registered") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if w := ss.wrote(); len(w) != 0 {
		t.Errorf("wrote: %v", w)
	}
}

func TestRun_NodesSuggestionsConfirm_UnknownID(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "cccccccccccccccc", "--yes", "--server", ss.srv.URL}, &stdout, &stderr)
	if code != ExitUserError || !strings.Contains(stderr.String(), "no current suggestion") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
}

func TestRun_NodesSuggestionsConfirm_ServerConflictIsAUserError(t *testing.T) {
	ss := newSuggestionServer(t, http.StatusConflict, `{"error":"a member of this group is no longer registered; refresh and review it again"}`)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--yes", "--server", ss.srv.URL}, &stdout, &stderr)
	if code != ExitUserError || !strings.Contains(stderr.String(), "no longer registered") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
}

func TestRun_NodesSuggestionsDismissAndRestore(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "dismiss", "aaaaaaaaaaaaaaaa", "--server", ss.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("dismiss exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	if code := Run([]string{"nodes", "suggestions", "restore", "aaaaaaaaaaaaaaaa", "--json", "--server", ss.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("restore exit %d: %s", code, stderr.String())
	}
	w := ss.wrote()
	if len(w) != 2 || w[0] != "POST /admin/v1/replica-suggestions/aaaaaaaaaaaaaaaa/dismiss" || w[1] != "DELETE /admin/v1/replica-suggestions/aaaaaaaaaaaaaaaa/dismiss" {
		t.Errorf("writes = %v", w)
	}
	if !strings.Contains(stdout.String(), `"dismissed": false`) {
		t.Errorf("restore --json = %s", stdout.String())
	}
}

func TestRun_NodesSuggestions_NeedsID(t *testing.T) {
	ss := newSuggestionServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	for _, sub := range []string{"confirm", "dismiss", "restore"} {
		if code := Run([]string{"nodes", "suggestions", sub, "--server", ss.srv.URL}, &stdout, &stderr); code != ExitUserError {
			t.Errorf("%s without an id: exit %d", sub, code)
		}
	}
}
