package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const twoSoleImpactJSON = `{"node":"gpu-0","sole_replica":{"models":[{"model":"org/model-a","runtime":"vllm"},{"model":"m b&c=d+e","runtime":"vllm"}],"cold_load_only":[],"unknown":[]}}`

func postQuery(t *testing.T, s *safetyServer) url.Values {
	t.Helper()
	posts := s.posts()
	if len(posts) != 1 {
		t.Fatalf("want one POST, got %v", posts)
	}
	_, raw, _ := strings.Cut(posts[0], "?")
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query %q: %v", raw, err)
	}
	return v
}

func TestAck_TTYAcceptSendsAckOfPrintedModels(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := newSafetyServer(t, twoSoleImpactJSON)
			withSafetyTTY(t, true, "y\n")
			if code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0"); code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			q := postQuery(t, s)
			if q.Get("force") != "true" {
				t.Errorf("force missing: %v", q)
			}
			got := q["acknowledge_sole"]
			if len(got) != 2 || got[0] != "org/model-a" || got[1] != "m b&c=d+e" {
				t.Errorf("acknowledge_sole = %q", got)
			}
		})
	}
}

func TestAck_ExplicitForceSendsBareForce(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := newSafetyServer(t, soleImpactJSON)
			withSafetyTTY(t, false, "")
			if code, _, stderr := runSafetyCLI(t, s, "runtime", action, "gpu-0", "--force"); code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			q := postQuery(t, s)
			if _, has := q["acknowledge_sole"]; has || q.Get("force") != "true" {
				t.Errorf("--force must send a bare force: %v", q)
			}
		})
	}
}

func TestAck_TTYDeclineSendsNothing(t *testing.T) {
	s := newSafetyServer(t, soleImpactJSON)
	withSafetyTTY(t, true, "n\n")
	runSafetyCLI(t, s, "runtime", "drain", "gpu-0")
	if len(s.posts()) != 0 {
		t.Errorf("posts: %v", s.posts())
	}
}

func TestAck_EmptyListStillSendsKey(t *testing.T) {
	got, err := soleActionQuery(false, true, []string{})
	if err != nil || got != "?acknowledge_sole=&force=true" {
		t.Fatalf("query = %q, err %v", got, err)
	}
	got, err = soleActionQuery(true, true, nil)
	if err != nil || got != "?acknowledge_replica=true&force=true" {
		t.Fatalf("bare force query = %q, err %v", got, err)
	}
	got, err = soleActionQuery(false, false, []string{"x"})
	if err != nil || got != "" {
		t.Fatalf("ack without force must not be sent: %q, err %v", got, err)
	}
}

func TestAck_QueryEncodingRoundTrips(t *testing.T) {
	names := []string{"a+b&c=d#e;f%41/g:h", "café-中", "x y,z"}
	got, err := soleActionQuery(false, true, names)
	if err != nil {
		t.Fatal(err)
	}
	v, err := url.ParseQuery(strings.TrimPrefix(got, "?"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v["acknowledge_sole"]) != len(names) {
		t.Fatalf("round trip: %q", v["acknowledge_sole"])
	}
	for i, n := range names {
		if v["acknowledge_sole"][i] != n {
			t.Errorf("name %d = %q, want %q", i, v["acknowledge_sole"][i], n)
		}
	}
}

// One case just under and one just over the encoded size limit.
func TestAck_QuerySizeLimit(t *testing.T) {
	under := []string{strings.Repeat("a", maxSoleQueryBytes-len("force=true&acknowledge_sole="))}
	q, err := soleActionQuery(false, true, under)
	if err != nil {
		t.Fatalf("just under the limit must be accepted: %v", err)
	}
	if len(q)-1 > maxSoleQueryBytes {
		t.Fatalf("query is %d bytes", len(q)-1)
	}
	oneOver := []string{strings.Repeat("a", maxSoleQueryBytes-len("force=true&acknowledge_sole=")+1)}
	if _, err := soleActionQuery(false, true, oneOver); err == nil {
		t.Fatal("one byte over the limit must be refused")
	}
	over := []string{strings.Repeat("a", maxSoleQueryBytes)}
	if _, err := soleActionQuery(false, true, over); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("over the limit must be refused with a pointer to --force, got %v", err)
	}
}

func TestAck_OversizedListRefusedBeforeAnyPost(t *testing.T) {
	var models []string
	for i := 0; i < 400; i++ {
		models = append(models, `{"model":"`+strings.Repeat("m", 30)+`","runtime":"vllm"}`)
	}
	impact := `{"node":"gpu-0","sole_replica":{"models":[` + strings.Join(models, ",") + `],"cold_load_only":[],"unknown":[]}}`
	s := newSafetyServer(t, impact)
	withSafetyTTY(t, true, "y\n")
	code, _, stderr := runSafetyCLI(t, s, "runtime", "drain", "gpu-0")
	if code != ExitUserError || !strings.Contains(stderr, "too large") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "Proceed?") {
		t.Errorf("the operator must not be asked to confirm a list that cannot be sent: %s", stderr)
	}
	if len(s.posts()) != 0 {
		t.Errorf("posts: %v", s.posts())
	}
}

func TestAck_StaleAckPrintsFreshListAndNeverRetries(t *testing.T) {
	var mu sync.Mutex
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte(soleImpactJSON))
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"drain was not done: the models with no other server changed since you reviewed them: \"org/new\"; review and retry.","code":"sole_replica","unacknowledged":["org/new"],"sole_replica":{"models":[{"model":"org/model-a","runtime":"vllm"},{"model":"org/new","runtime":"vllm"}],"cold_load_only":[],"unknown":[],"forced":false}}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	for _, action := range []string{"drain", "restart"} {
		withSafetyTTY(t, true, "y\n")
		var stdout, stderr strings.Builder
		code := Run([]string{"runtime", action, "gpu-0", "--server", srv.URL}, &stdout, &stderr)
		if code != ExitUserError {
			t.Fatalf("%s: exit %d", action, code)
		}
		out := stderr.String()
		if !strings.Contains(out, "The models with no other server are now:") || !strings.Contains(out, "  org/new (vllm)\n") {
			t.Errorf("%s: the fresh list is missing: %q", action, out)
		}
		if !strings.Contains(out, "changed since you reviewed them") || !strings.Contains(out, "org/new") || !strings.Contains(out, "Run the command again") {
			t.Errorf("%s: stderr = %q", action, out)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: stdout = %q", action, stdout.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 {
		t.Errorf("one POST per command and no retry, got %d", posts)
	}
}

func TestAck_PlainConflictDoesNotPrintFreshListBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte(noImpactJSON))
			return
		}
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"some other conflict","code":"replica_member"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	withSafetyTTY(t, false, "")
	var stdout, stderr strings.Builder
	code := Run([]string{"runtime", "restart", "gpu-0", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitUserError || strings.Contains(stderr.String(), "Run the command again") || !strings.Contains(stderr.String(), "some other conflict") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
