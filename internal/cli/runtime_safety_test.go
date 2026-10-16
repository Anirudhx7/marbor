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

const (
	soleImpactJSON = `{"node":"gpu-0","sole_replica":{"models":[{"model":"org/model-a","runtime":"vllm"}],"cold_load_only":[],"unknown":[]}}`
	noImpactJSON   = `{"node":"gpu-0","sole_replica":{"models":[],"cold_load_only":[],"unknown":[]}}`
	coldImpactJSON = `{"node":"gpu-0","sole_replica":{"models":[],"cold_load_only":[{"model":"llama3","runtime":"ollama","peers":["gpu-1"]}],"unknown":[{"node":"gpu-2","model":"llama3","reason":"node has not been polled yet"}]}}`
)

// safetyServer answers the sole-replica check with impact and records every
// request, so a test can assert what the CLI sent. Mutating calls answer 200
// unless a forced call is required and force is missing.
type safetyServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newSafetyServer(t *testing.T, impact string) *safetyServer {
	t.Helper()
	s := &safetyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte(impact))
			return
		}
		w.Write([]byte(`{"node":"gpu-0","draining":true,"ok":true}`))
	}))
	t.Cleanup(s.Close)
	withTempConfigDir(t)
	mustSaveSession(t, s.URL, "tok")
	return s
}

func (s *safetyServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *safetyServer) posts() []string {
	var out []string
	for _, r := range s.seen() {
		if strings.HasPrefix(r, "POST ") {
			out = append(out, r)
		}
	}
	return out
}

// withTTY makes stdin look like a terminal that answers answer, restoring the
// real values afterwards.
func withSafetyTTY(t *testing.T, tty bool, answer string) {
	t.Helper()
	origTTY, origReader := stdinIsTTY, stdinReader
	stdinIsTTY = func() bool { return tty }
	stdinReader = strings.NewReader(answer)
	t.Cleanup(func() { stdinIsTTY, stdinReader = origTTY, origReader })
}

func runSafetyCLI(t *testing.T, s *safetyServer, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(append(args, "--server", s.URL), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRuntimeSafety_Matrix(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action+"/tty accept proceeds with force", func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, true, "y\n")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stderr, "Proceed? [y/N]") || !strings.Contains(stderr, "org/model-a") {
				t.Errorf("prompt should list the model: %q", stderr)
			}
			if strings.Contains(stderr, "cannot be undone") {
				t.Errorf("prompt must not claim the action cannot be undone: %q", stderr)
			}
			if posts := s.posts(); len(posts) != 1 || !strings.Contains(posts[0], "force=true") {
				t.Errorf("want one forced POST, got %v", posts)
			}
		})
		t.Run(action+"/tty decline sends nothing", func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, true, "n\n")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0")
			if code != ExitUserError || !strings.Contains(stderr, "aborted") {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if len(s.posts()) != 0 {
				t.Errorf("a declined action must not POST: %v", s.posts())
			}
		})
		t.Run(action+"/non-tty lists models and exits 1", func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, false, "")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0")
			if code != ExitUserError {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stderr, "org/model-a") || !strings.Contains(stderr, "--force") {
				t.Errorf("error should name the model and --force: %q", stderr)
			}
			if len(s.posts()) != 0 {
				t.Errorf("must not POST without --force: %v", s.posts())
			}
		})
		t.Run(action+"/force skips the prompt", func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, false, "")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--force")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if posts := s.posts(); len(posts) != 1 || !strings.Contains(posts[0], "force=true") {
				t.Errorf("want one forced POST, got %v", posts)
			}
		})
		t.Run(action+"/no sole models sends no force even with --force", func(t *testing.T) {
			s := newSafetyServer(t, noImpactJSON)
			withSafetyTTY(t, false, "")
			code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--force")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if posts := s.posts(); len(posts) != 1 || strings.Contains(posts[0], "force") {
				t.Errorf("want one unforced POST, got %v", posts)
			}
		})
		t.Run(action+"/cold and unknown print but proceed", func(t *testing.T) {
			s := newSafetyServer(t, coldImpactJSON)
			withSafetyTTY(t, false, "")
			code, stdout, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stderr, "cold-load on gpu-1") || !strings.Contains(stderr, "could not verify") {
				t.Errorf("stderr should carry both notes: %q", stderr)
			}
			if strings.Contains(stdout, "cold-load") {
				t.Errorf("notes belong on stderr, stdout = %q", stdout)
			}
			if posts := s.posts(); len(posts) != 1 || strings.Contains(posts[0], "force") {
				t.Errorf("want one unforced POST, got %v", posts)
			}
		})
		t.Run(action+"/dry-run only reads", func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, false, "")
			code, stdout, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--dry-run")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if len(s.posts()) != 0 {
				t.Errorf("dry run must not POST: %v", s.posts())
			}
			if !strings.Contains(stdout, "org/model-a") || !strings.Contains(stdout, "nothing changed") {
				t.Errorf("stdout = %q", stdout)
			}
		})
		t.Run(action+"/json includes sole_replica", func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, false, "")
			code, stdout, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--force", "--json")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			var out struct {
				SoleReplica struct {
					Models []map[string]string `json:"models"`
				} `json:"sole_replica"`
			}
			if err := json.Unmarshal([]byte(stdout), &out); err != nil {
				t.Fatalf("decode %q: %v", stdout, err)
			}
			if len(out.SoleReplica.Models) != 1 || out.SoleReplica.Models[0]["model"] != "org/model-a" {
				t.Errorf("sole_replica.models = %+v (stdout %s)", out.SoleReplica.Models, stdout)
			}
		})
	}
}

func TestRuntimeSafety_DryRunJSONAndCheckFailure(t *testing.T) {
	t.Run("dry-run json", func(t *testing.T) {
		s := newSafetyServer(t, soleImpactJSON)
		code, stdout, stderr := runSafetyCLI(t, s, "runtime", "restart", "gpu-0", "--dry-run", "--json")
		if code != ExitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		if !strings.Contains(stdout, `"dry_run": true`) || !strings.Contains(stdout, "org/model-a") {
			t.Errorf("stdout = %s", stdout)
		}
	})
	t.Run("an unreadable check is a note and the server decides", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"not found"}`))
				return
			}
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"restarting the runtime on node \"gpu-0\" would leave 1 model(s) with no other node that can serve them","code":"sole_replica"}`))
		}))
		defer srv.Close()
		withTempConfigDir(t)
		mustSaveSession(t, srv.URL, "tok")
		var stdout, stderr bytes.Buffer
		code := Run([]string{"runtime", "restart", "gpu-0", "--server", srv.URL}, &stdout, &stderr)
		if code != ExitUserError {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "could not check") || !strings.Contains(stderr.String(), "would leave 1 model(s)") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})
	t.Run("dry-run with an unreadable check fails", func(t *testing.T) {
		withTempConfigDir(t)
		mustSaveSession(t, "http://127.0.0.1:1", "tok")
		var stdout, stderr bytes.Buffer
		code := Run([]string{"runtime", "drain", "gpu-0", "--dry-run", "--server", "http://127.0.0.1:1"}, &stdout, &stderr)
		if code != ExitServerError {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	})
}

func TestRuntimeSafety_StopAndStartAreNotChecked(t *testing.T) {
	for _, action := range []string{"start", "stop"} {
		s := newSafetyServer(t, soleImpactJSON)
		withSafetyTTY(t, false, "")
		code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0")
		if code != ExitOK {
			t.Fatalf("%s: exit %d: %s", action, code, stderr)
		}
		for _, r := range s.seen() {
			if strings.HasPrefix(r, "GET ") {
				t.Errorf("%s must not run the check: %v", action, s.seen())
			}
		}
	}
}

func TestRuntimeSafety_FlagsOnlyOnDrainAndRestart(t *testing.T) {
	s := newSafetyServer(t, noImpactJSON)
	for _, action := range []string{"start", "stop"} {
		if code, _, _ := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--force"); code != ExitUserError {
			t.Errorf("%s --force: exit %d, want a usage error", action, code)
		}
	}
}
