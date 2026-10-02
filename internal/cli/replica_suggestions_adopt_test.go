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

// A list with a group that contradicts a declaration (gpu-b declares itself
// head, gpu-a declares nothing), a complete one, and the usual incomplete one.
const adoptListBody = `{
  "suggestions": [
    {"fingerprint":"dddddddddddddddd","launcher":"vllm-mp","runtime":"vllm","state":"contradicts_declared",
     "reason":"a member already declares a different replica membership; clear or fix it first",
     "head":"gpu-a","members":["gpu-a","gpu-b"],"evidence":[],"missing":[],
     "declared":[{"node":"gpu-b","head":"gpu-b","members":["gpu-a","gpu-b"]}],"confirmable":false,"dismissed":false},
    {"fingerprint":"aaaaaaaaaaaaaaaa","launcher":"vllm-mp","runtime":"vllm","state":"complete","reason":"",
     "head":"gpu-c","members":["gpu-c","gpu-d"],"evidence":[],"missing":[],"declared":[],"confirmable":true,"dismissed":false}
  ],
  "coverage": [],
  "dismissedCount": 0
}`

type adoptServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string // bodies of non-GET requests
	calls  []string
}

func newAdoptServer(t *testing.T, confirmStatus int, confirmBody string) *adoptServer {
	t.Helper()
	as := &adoptServer{}
	as.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		as.mu.Lock()
		as.calls = append(as.calls, r.Method+" "+r.URL.RequestURI())
		if r.Method != http.MethodGet {
			as.bodies = append(as.bodies, string(b))
		}
		as.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(adoptListBody))
		case strings.HasSuffix(r.URL.Path, "/confirm"):
			w.WriteHeader(confirmStatus)
			w.Write([]byte(confirmBody))
		default:
			w.Write([]byte(`{"fingerprint":"x","dismissed":true}`))
		}
	}))
	t.Cleanup(as.srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, as.srv.URL, "tok")
	return as
}

func (as *adoptServer) writes() int {
	as.mu.Lock()
	defer as.mu.Unlock()
	return len(as.bodies)
}

func TestRun_NodesSuggestionsConfirmAdopt_SendsSnapshotWhatThePromptShowed(t *testing.T) {
	as := newAdoptServer(t, 200, confirmOKBody)
	withTTY(t, true, "y\n")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--adopt", "--server", as.srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	prompt := stderr.String()
	for _, want := range []string{"overwrite", "gpu-a", "gpu-b", `declared now`, `head "gpu-b"`, "nothing declared", `detected`, `head "gpu-a"`, "Nothing is restarted", "--replica-members"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if as.writes() != 1 {
		t.Fatalf("writes = %v", as.bodies)
	}
	var sent struct {
		Adopt            bool                     `json:"adopt"`
		DeclaredSnapshot []NodeSuggestionDeclared `json:"declaredSnapshot"`
	}
	if err := json.Unmarshal([]byte(as.bodies[0]), &sent); err != nil {
		t.Fatalf("body %q: %v", as.bodies[0], err)
	}
	if !sent.Adopt || len(sent.DeclaredSnapshot) != 1 || sent.DeclaredSnapshot[0].Node != "gpu-b" ||
		sent.DeclaredSnapshot[0].Head != "gpu-b" || strings.Join(sent.DeclaredSnapshot[0].Members, ",") != "gpu-a,gpu-b" {
		t.Errorf("sent = %+v", sent)
	}
	if !strings.Contains(stdout.String(), "replica group declared") {
		t.Errorf("stdout = %s", stdout.String())
	}
}

func TestRun_NodesSuggestionsConfirmAdopt_YesSkipsPromptAndAnswerNoAborts(t *testing.T) {
	as := newAdoptServer(t, 200, confirmOKBody)
	withTTY(t, false, "")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--adopt", "--yes", "--server", as.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "Proceed?") {
		t.Error("--yes must not prompt")
	}
	if as.writes() != 1 || !strings.Contains(as.bodies[0], `"adopt":true`) {
		t.Errorf("bodies = %v", as.bodies)
	}

	as2 := newAdoptServer(t, 200, confirmOKBody)
	withTTY(t, true, "n\n")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--adopt", "--server", as2.srv.URL}, &stdout, &stderr); code != ExitUserError {
		t.Fatalf("exit %d", code)
	}
	if as2.writes() != 0 {
		t.Errorf("aborted adopt wrote: %v", as2.bodies)
	}

	as3 := newAdoptServer(t, 200, confirmOKBody)
	withTTY(t, false, "")
	stderr.Reset()
	if code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--adopt", "--server", as3.srv.URL}, &stdout, &stderr); code != ExitUserError || !strings.Contains(stderr.String(), "without --yes") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if as3.writes() != 0 {
		t.Errorf("refused adopt wrote: %v", as3.bodies)
	}
}

func TestRun_NodesSuggestionsConfirm_WithoutAdoptStillRefusesAContradiction(t *testing.T) {
	as := newAdoptServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--yes", "--server", as.srv.URL}, &stdout, &stderr)
	if code != ExitUserError || !strings.Contains(stderr.String(), "cannot be confirmed") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--adopt") {
		t.Errorf("the refusal should point at --adopt: %s", stderr.String())
	}
	if as.writes() != 0 {
		t.Errorf("wrote: %v", as.bodies)
	}
}

func TestRun_NodesSuggestionsConfirm_PlainConfirmSendsNoBody(t *testing.T) {
	as := newAdoptServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--yes", "--server", as.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if as.writes() != 1 || as.bodies[0] != "" {
		t.Errorf("bodies = %q", as.bodies)
	}
}

func TestRun_NodesSuggestionsConfirmAdopt_OnACompleteGroupAsksToRerunWithoutAdopt(t *testing.T) {
	as := newAdoptServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "aaaaaaaaaaaaaaaa", "--adopt", "--yes", "--server", as.srv.URL}, &stdout, &stderr)
	if code != ExitUserError || !strings.Contains(stderr.String(), "without --adopt") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if as.writes() != 0 {
		t.Errorf("wrote: %v", as.bodies)
	}
}

func TestRun_NodesSuggestionsConfirmAdopt_ServerRefusalIsPrinted(t *testing.T) {
	as := newAdoptServer(t, http.StatusConflict,
		`{"error":"these nodes outside the group declare a replica membership that includes a member of it: gpu-z; fix or clear their declarations first","suggestion":{"fingerprint":"dddddddddddddddd","state":"contradicts_declared"}}`)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--adopt", "--yes", "--server", as.srv.URL}, &stdout, &stderr)
	if code != ExitUserError || !strings.Contains(stderr.String(), "gpu-z") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "without --adopt") {
		t.Errorf("a still-contradicting group must not tell the operator to rerun without --adopt: %s", stderr.String())
	}
}

func TestRun_NodesSuggestionsConfirmAdopt_NowCompleteTellsOperatorToRerun(t *testing.T) {
	as := newAdoptServer(t, http.StatusConflict,
		`{"error":"this group no longer conflicts with a declaration and is ready to confirm; confirm it without adopting","suggestion":{"fingerprint":"dddddddddddddddd","state":"complete"}}`)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "suggestions", "confirm", "dddddddddddddddd", "--adopt", "--yes", "--server", as.srv.URL}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no longer conflicts") || !strings.Contains(stderr.String(), "now complete") || !strings.Contains(stderr.String(), "without --adopt") {
		t.Errorf("stderr = %s", stderr.String())
	}
}

func TestRun_NodesSuggestionsDismiss_SendsNoBody(t *testing.T) {
	as := newAdoptServer(t, 200, confirmOKBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "suggestions", "dismiss", "dddddddddddddddd", "--server", as.srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if as.writes() != 1 || as.bodies[0] != "" {
		t.Errorf("bodies = %q", as.bodies)
	}
}
