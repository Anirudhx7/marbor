package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// prefixLocalityServer serves the given body for the stats endpoint.
func prefixLocalityServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/admin/prefix-locality/stats" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("expected Authorization %q, got %q", "Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	return srv
}

func runPrefixLocality(t *testing.T, srv *httptest.Server, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"prefix-locality", "stats", "--server", srv.URL}, extra...)
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRun_PrefixLocalityStats_Human(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{"enabled":true,"hits":142,"misses":58,"hit_rate":0.71}`)
	code, out, errOut := runPrefixLocality(t, srv)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
	}
	want := "enabled=true hits=142 misses=58 hit_rate=71.0%"
	if !strings.Contains(out, want) {
		t.Errorf("expected %q in output, got %q", want, out)
	}
}

func TestRun_PrefixLocalityStats_NoSamplesShowsNA(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{"enabled":true,"hits":0,"misses":0,"hit_rate":0}`)
	code, out, errOut := runPrefixLocality(t, srv)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
	}
	if !strings.Contains(out, "hit_rate=n/a (no requests yet)") {
		t.Errorf("expected n/a hit rate, got %q", out)
	}
	if strings.Contains(out, "0.0%") {
		t.Errorf("must not print a fabricated 0.0%% ratio, got %q", out)
	}
}

func TestRun_PrefixLocalityStats_DisabledWithCountsNotes(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{"enabled":false,"hits":3,"misses":1,"hit_rate":0.75}`)
	code, out, errOut := runPrefixLocality(t, srv)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
	}
	if !strings.Contains(out, "enabled=false") || !strings.Contains(out, "hit_rate=75.0%") {
		t.Errorf("expected disabled line with 75.0%%, got %q", out)
	}
	if !strings.Contains(out, "feature currently disabled; counts are from earlier use") {
		t.Errorf("expected a disabled note, got %q", out)
	}
}

func TestRun_PrefixLocalityStats_DisabledNoCountsNoNote(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{"enabled":false,"hits":0,"misses":0,"hit_rate":0}`)
	code, out, errOut := runPrefixLocality(t, srv)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
	}
	if !strings.Contains(out, "enabled=false") || !strings.Contains(out, "n/a") {
		t.Errorf("expected enabled=false and n/a, got %q", out)
	}
	if strings.Contains(out, "currently disabled") {
		t.Errorf("no note expected when there are no counts, got %q", out)
	}
}

func TestRun_PrefixLocalityStats_JSON(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{"enabled":true,"hits":0,"misses":0,"hit_rate":0}`)
	code, out, errOut := runPrefixLocality(t, srv, "--json")
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out)
	}
	if len(got) != 4 {
		t.Errorf("expected exactly 4 keys, got %v", got)
	}
	for _, k := range []string{"enabled", "hits", "misses", "hit_rate"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %v", k, got)
		}
	}
	if got["hit_rate"] != float64(0) {
		t.Errorf("hit_rate must pass through as 0, got %v", got["hit_rate"])
	}
}

func TestRun_PrefixLocalityStats_JSONLargeCountsExact(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{"enabled":true,"hits":18446744073709551615,"misses":1,"hit_rate":0.5}`)
	code, out, errOut := runPrefixLocality(t, srv, "--json")
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
	}
	var got struct {
		Enabled bool    `json:"enabled"`
		Hits    uint64  `json:"hits"`
		Misses  uint64  `json:"misses"`
		HitRate float64 `json:"hit_rate"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out)
	}
	if !got.Enabled || got.Hits != 18446744073709551615 || got.Misses != 1 || got.HitRate != 0.5 {
		t.Errorf("values not preserved exactly: %+v", got)
	}
}

func TestRun_PrefixLocalityStats_RateFormatting(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"rounds", `{"enabled":true,"hits":7149,"misses":2851,"hit_rate":0.7149}`, "hit_rate=71.5%"},
		{"full", `{"enabled":true,"hits":4,"misses":0,"hit_rate":1.0}`, "hit_rate=100.0%"},
		{"no counts ignores server rate", `{"enabled":true,"hits":0,"misses":0,"hit_rate":0.9}`, "hit_rate=n/a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := prefixLocalityServer(t, 200, tc.body)
			code, out, errOut := runPrefixLocality(t, srv)
			if code != ExitOK {
				t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, errOut)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q in %q", tc.want, out)
			}
		})
	}
}

func TestRun_PrefixLocalityStats_ServerFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500", 500, `{"error":"boom"}`},
		{"empty 200 body", 200, ``},
		{"empty object", 200, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := prefixLocalityServer(t, tc.status, tc.body)
			code, out, errOut := runPrefixLocality(t, srv)
			if code != ExitServerError {
				t.Fatalf("expected exit %d, got %d", ExitServerError, code)
			}
			if strings.TrimSpace(errOut) == "" {
				t.Error("expected a message on stderr")
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("expected empty stdout, got %q", out)
			}
		})
	}
}

func TestRun_PrefixLocalityStats_Unreachable(t *testing.T) {
	srv := prefixLocalityServer(t, 200, `{}`)
	srv.Close()
	code, _, errOut := runPrefixLocality(t, srv)
	if code != ExitServerError {
		t.Fatalf("expected exit %d, got %d", ExitServerError, code)
	}
	if strings.TrimSpace(errOut) == "" {
		t.Error("expected a message on stderr")
	}
}

func TestRun_PrefixLocalityStats_NoSession(t *testing.T) {
	t.Setenv("MARBOR_USERNAME", "")
	t.Setenv("MARBOR_PASSWORD", "")
	withTempConfigDir(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"prefix-locality", "stats", "--server", "http://127.0.0.1:1"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d (missing session is a user error), got %d", ExitUserError, code)
	}
	if !strings.Contains(stderr.String(), "authentication required") {
		t.Errorf("expected an authentication hint, got %q", stderr.String())
	}
}

func TestRun_PrefixLocalityStats_Unauthorized(t *testing.T) {
	srv := prefixLocalityServer(t, 401, `{"error":"unauthorized"}`)
	code, _, _ := runPrefixLocality(t, srv)
	if code != ExitAuthError {
		t.Fatalf("expected exit %d, got %d", ExitAuthError, code)
	}
}

func TestRegistry_PrefixLocalityStatsCommand(t *testing.T) {
	var found *Command
	for _, c := range root().Sub {
		if c.Name == "prefix-locality" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("prefix-locality command missing from the registry")
	}
	if !found.NeedsAuth {
		t.Error("prefix-locality must require auth")
	}
	var stats *Command
	for _, s := range found.Sub {
		if s.Name == "stats" {
			stats = s
		}
	}
	if stats == nil || stats.Run == nil || !stats.NeedsAuth {
		t.Fatal("prefix-locality stats missing, not runnable, or not auth-gated")
	}
}
