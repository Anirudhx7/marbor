package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStripControl(t *testing.T) {
	cases := map[string]string{
		"plain/model:7b":      "plain/model:7b",
		"evil\x1b[2Jname":     "evil[2Jname",
		"two\nlines\rand\x07": "twolinesand",
		"tab\there":           "tabhere",
		"café-中文":             "café-中文",
	}
	for in, want := range cases {
		if got := stripControl(in); got != want {
			t.Errorf("stripControl(%q) = %q, want %q", in, got, want)
		}
	}
}

const hostileImpactJSON = `{"node":"gpu-0","sole_replica":{"models":[{"model":"evil\u001b[2Jmodel\nfake line","runtime":"vl\u001b]0;llm"}],"cold_load_only":[],"unknown":[]}}`

// A hostile model or runtime name never reaches the terminal with control
// characters, but the acknowledged name is sent exactly as the server gave it.
func TestAck_PromptStripsControlCharactersButAckKeepsName(t *testing.T) {
	s := newSafetyServer(t, hostileImpactJSON)
	withSafetyTTY(t, true, "y\n")
	code, _, stderr := runSafetyCLI(t, s, "runtime", "drain", "gpu-0")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.ContainsRune(stderr, 0x1b) || strings.Contains(stderr, "model\nfake line") {
		t.Errorf("control characters reached the terminal: %q", stderr)
	}
	if got := postQuery(t, s)["acknowledge_sole"]; len(got) != 1 || got[0] != "evil\x1b[2Jmodel\nfake line" {
		t.Errorf("acknowledge_sole = %q", got)
	}
}

func TestAck_NoTTYErrorStripsControlCharacters(t *testing.T) {
	s := newSafetyServer(t, hostileImpactJSON)
	withSafetyTTY(t, false, "")
	code, _, stderr := runSafetyCLI(t, s, "runtime", "restart", "gpu-0")
	if code != ExitUserError || strings.ContainsRune(stderr, 0x1b) || strings.Contains(stderr, "model\nfake line") {
		t.Fatalf("exit %d: %q", code, stderr)
	}
}

func TestAck_StaleListStripsControlCharacters(t *testing.T) {
	err := &CLIError{Code: ExitUserError, Msg: "stale", Raw: []byte(`{"code":"sole_replica","unacknowledged":["x"],"sole_replica":{"models":[{"model":"a\u001b[31mb","runtime":"v\nllm"}]}}`)}
	var stderr strings.Builder
	reportSoleActionError(err, &stderr)
	if strings.ContainsRune(stderr.String(), 0x1b) || !strings.Contains(stderr.String(), "a[31mb (vllm)") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// Only the sole-replica 409 gets the fresh-list block.
func TestAck_ReportSoleActionErrorRequiresSoleReplicaCode(t *testing.T) {
	err := &CLIError{Code: ExitUserError, Msg: "other", Raw: []byte(`{"code":"replica_member","unacknowledged":["x"],"sole_replica":{"models":[{"model":"m","runtime":"vllm"}]}}`)}
	var stderr strings.Builder
	reportSoleActionError(err, &stderr)
	if strings.Contains(stderr.String(), "no other server") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// --force on a terminal never prompts and sends a bare force.
func TestAck_ForceOnTTYDoesNotPromptAndSendsBareForce(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, true, "")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--force")
			if code != ExitOK || strings.Contains(stderr, "Proceed?") {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			q := postQuery(t, s)
			if _, has := q["acknowledge_sole"]; has || q.Get("force") != "true" {
				t.Errorf("query = %v", q)
			}
		})
	}
}

// A check that cannot be read sends the action on, carrying only the operator's
// own --force and never an acknowledged list.
func TestAck_UnreadableCheckSendsBareForceOnlyWithFlag(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		for _, force := range []bool{false, true} {
			t.Run(action, func(t *testing.T) {
				var posted string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						w.WriteHeader(http.StatusNotFound)
						w.Write([]byte(`{"error":"not found"}`))
						return
					}
					posted = r.URL.RawQuery
					w.Write([]byte(`{"ok":true,"draining":true}`))
				}))
				defer srv.Close()
				withTempConfigDir(t)
				mustSaveSession(t, srv.URL, "tok")
				withSafetyTTY(t, true, "")
				args := []string{"runtime", action, "gpu-0", "--server", srv.URL}
				if force {
					args = append(args, "--force")
				}
				var stdout, stderr strings.Builder
				if code := Run(args, &stdout, &stderr); code != ExitOK {
					t.Fatalf("exit %d: %s", code, stderr.String())
				}
				want := ""
				if force {
					want = "force=true"
				}
				if posted != want {
					t.Errorf("query = %q, want %q", posted, want)
				}
			})
		}
	}
}

// With nothing sole there is no prompt and no query at all.
func TestAck_NoSoleModelsOnTTYSendsPlainPost(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := newSafetyServer(t, noImpactJSON)
			withSafetyTTY(t, true, "")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0")
			if code != ExitOK || strings.Contains(stderr, "Proceed?") {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if posts := s.posts(); len(posts) != 1 || strings.Contains(posts[0], "?") {
				t.Errorf("posts = %v", posts)
			}
		})
	}
}

// A declined restart sends nothing, like a declined drain.
func TestAck_TTYDeclineOnRestartSendsNothing(t *testing.T) {
	s := newSafetyServer(t, soleImpactJSON)
	withSafetyTTY(t, true, "n\n")
	if code, _, _ := runSafetyCLI(t, s, "runtime", "restart", "gpu-0"); code != ExitUserError {
		t.Errorf("exit %d", code)
	}
	if len(s.posts()) != 0 {
		t.Errorf("posts: %v", s.posts())
	}
}

func conflictServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()}
}

// A 409 body is read only up to the cap.
func TestConflictBody_IsCapped(t *testing.T) {
	c := conflictServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(strings.Repeat("x", 2*maxConflictBodyBytes)))
	})
	_, err := c.DrainNode("gpu-0", "", -1, false, nil)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v", err)
	}
	if len(cliErr.Raw) != maxConflictBodyBytes {
		t.Errorf("Raw is %d bytes, want %d", len(cliErr.Raw), maxConflictBodyBytes)
	}
}

// A body cut off mid-read keeps the status and message but drops Raw, so a
// half-read list is never trusted.
func TestConflictBody_ReadErrorKeepsStatusAndDropsRaw(t *testing.T) {
	c := conflictServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"cut short"`))
	})
	_, err := c.DrainNode("gpu-0", "", -1, false, nil)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v", err)
	}
	if cliErr.Code != ExitUserError || cliErr.Msg == "" || cliErr.Raw != nil {
		t.Errorf("code %d msg %q raw %q", cliErr.Code, cliErr.Msg, cliErr.Raw)
	}
}
