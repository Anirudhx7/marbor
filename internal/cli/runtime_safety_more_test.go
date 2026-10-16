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

// scriptedServer answers GET with getStatus/getBody and every other method with
// postStatus/postBody, recording the POSTs it receives.
type scriptedServer struct {
	getStatus, postStatus int
	getBody, postBody     string
	mu                    sync.Mutex
	posts                 []string
}

func startScripted(t *testing.T, s *scriptedServer) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(s.getStatus)
			w.Write([]byte(s.getBody))
			return
		}
		s.mu.Lock()
		s.posts = append(s.posts, r.URL.RequestURI())
		s.mu.Unlock()
		w.WriteHeader(s.postStatus)
		w.Write([]byte(s.postBody))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	return srv.URL
}

func runScripted(url string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(append(args, "--server", url), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func decodeSole(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	sr, _ := out["sole_replica"].(map[string]any)
	return sr
}

func TestRuntimeSafety_ForceSurvivesAnUnreadableCheck(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := &scriptedServer{getStatus: 500, getBody: "{\"error\":\"boom\"}", postStatus: 200, postBody: "{\"ok\":true,\"draining\":true}"}
			url := startScripted(t, s)
			code, _, stderr := runScripted(url, "runtime", action, "gpu-0", "--force")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if len(s.posts) != 1 || !strings.Contains(s.posts[0], "force=true") {
				t.Errorf("an explicit --force must reach the server even when the check failed: %v", s.posts)
			}
			if !strings.Contains(stderr, "could not check") {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
	t.Run("without --force nothing is forced", func(t *testing.T) {
		s := &scriptedServer{getStatus: 500, getBody: "{}", postStatus: 200, postBody: "{\"ok\":true,\"draining\":true}"}
		url := startScripted(t, s)
		runScripted(url, "runtime", "drain", "gpu-0")
		if len(s.posts) != 1 || strings.Contains(s.posts[0], "force") {
			t.Errorf("posts = %v", s.posts)
		}
	})
}

func TestRuntimeSafety_JSONForcedFollowsWhatWasSent(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action+"/force with sole models and no server body", func(t *testing.T) {
			s := &scriptedServer{getStatus: 200, getBody: soleImpactJSON, postStatus: 200, postBody: "{\"ok\":true,\"draining\":true}"}
			url := startScripted(t, s)
			code, stdout, stderr := runScripted(url, "runtime", action, "gpu-0", "--force", "--json")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if sr := decodeSole(t, stdout); sr == nil || sr["forced"] != true {
				t.Errorf("forced must be true when force was sent for sole models: %s", stdout)
			}
		})
		t.Run(action+"/cold only is never forced", func(t *testing.T) {
			s := &scriptedServer{getStatus: 200, getBody: coldImpactJSON, postStatus: 200, postBody: "{\"ok\":true,\"draining\":true}"}
			url := startScripted(t, s)
			code, stdout, stderr := runScripted(url, "runtime", action, "gpu-0", "--force", "--json")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if sr := decodeSole(t, stdout); sr == nil || sr["forced"] != false {
				t.Errorf("forced must be false without sole models: %s", stdout)
			}
		})
		t.Run(action+"/server body wins", func(t *testing.T) {
			body := "{\"ok\":true,\"draining\":true,\"sole_replica\":{\"models\":[{\"model\":\"server-m\",\"runtime\":\"vllm\"}],\"cold_load_only\":[],\"unknown\":[],\"forced\":true}}"
			s := &scriptedServer{getStatus: 200, getBody: noImpactJSON, postStatus: 200, postBody: body}
			url := startScripted(t, s)
			code, stdout, stderr := runScripted(url, "runtime", action, "gpu-0", "--json")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			sr := decodeSole(t, stdout)
			if sr == nil || sr["forced"] != true || !strings.Contains(stdout, "server-m") {
				t.Errorf("the server body must be shown as is: %s", stdout)
			}
		})
		t.Run(action+"/empty impact omits the key", func(t *testing.T) {
			s := &scriptedServer{getStatus: 200, getBody: noImpactJSON, postStatus: 200, postBody: "{\"ok\":true,\"draining\":true}"}
			url := startScripted(t, s)
			code, stdout, stderr := runScripted(url, "runtime", action, "gpu-0", "--json")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if strings.Contains(stdout, "sole_replica") {
				t.Errorf("nothing to report must omit sole_replica: %s", stdout)
			}
		})
		t.Run(action+"/lists are never null", func(t *testing.T) {
			get := "{\"node\":\"gpu-0\",\"sole_replica\":{\"models\":[{\"model\":\"m\",\"runtime\":\"vllm\"}]}}"
			s := &scriptedServer{getStatus: 200, getBody: get, postStatus: 200, postBody: "{\"ok\":true,\"draining\":true}"}
			url := startScripted(t, s)
			_, stdout, _ := runScripted(url, "runtime", action, "gpu-0", "--force", "--json")
			sr := decodeSole(t, stdout)
			for _, k := range []string{"cold_load_only", "unknown"} {
				if arr, ok := sr[k].([]any); !ok || arr == nil {
					t.Errorf("%s must be an empty array: %s", k, stdout)
				}
			}
		})
	}
}

func TestRuntimeSafety_DryRunWinsOverForceAndStartStopRejectIt(t *testing.T) {
	s := newSafetyServer(t, soleImpactJSON)
	code, _, stderr := runSafetyCLI(t, s, "runtime", "restart", "gpu-0", "--force", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(s.posts()) != 0 {
		t.Errorf("--dry-run must not POST even with --force: %v", s.posts())
	}
	for _, action := range []string{"start", "stop"} {
		if code, _, _ := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--dry-run"); code != ExitUserError {
			t.Errorf("%s --dry-run: exit %d, want a usage error", action, code)
		}
	}
}

func TestRuntimeSafety_ServerBlocksAfterACleanCheck(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := &scriptedServer{
				getStatus: 200, getBody: noImpactJSON, postStatus: 409,
				postBody: "{\"error\":\"the model list changed: would leave 1 model(s) with no other node\",\"code\":\"sole_replica\"}",
			}
			url := startScripted(t, s)
			code, _, stderr := runScripted(url, "runtime", action, "gpu-0")
			if code != ExitUserError || !strings.Contains(stderr, "would leave 1 model(s)") {
				t.Errorf("exit %d stderr %q", code, stderr)
			}
		})
	}
}

func TestSoleReplicaForOutput(t *testing.T) {
	sole := &SoleReplicaImpact{Models: []SoleReplicaModel{{Model: "m"}}}
	if got := soleReplicaForOutput(nil, nil, true); got != nil {
		t.Errorf("nothing to show: %+v", got)
	}
	if got := soleReplicaForOutput(nil, &SoleReplicaImpact{}, true); got != nil {
		t.Errorf("an empty pre-check must be omitted: %+v", got)
	}
	got := soleReplicaForOutput(nil, sole, true)
	if got == nil || !got.Forced || got.ColdLoadOnly == nil || got.Unknown == nil {
		t.Errorf("forced sole pre-check: %+v", got)
	}
	if got := soleReplicaForOutput(nil, sole, false); got == nil || got.Forced {
		t.Errorf("unforced: %+v", got)
	}
	cold := &SoleReplicaImpact{ColdLoadOnly: []ColdLoadModel{{Model: "m"}}}
	if got := soleReplicaForOutput(nil, cold, true); got == nil || got.Forced {
		t.Errorf("force with no sole models is not forced: %+v", got)
	}
	server := &SoleReplicaBody{Forced: true}
	if got := soleReplicaForOutput(server, sole, false); got == nil || !got.Forced || got.Models == nil {
		t.Errorf("the server body wins and is normalised: %+v", got)
	}
}
