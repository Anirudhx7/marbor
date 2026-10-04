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
		if r.URL.Path != "/admin/prefix-locality/stats" {
			t.Errorf("unexpected path %s", r.URL.Path)
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
	_, out, _ := runPrefixLocality(t, srv)
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
	if stats == nil || !stats.NeedsAuth {
		t.Fatal("prefix-locality stats missing or not auth-gated")
	}
}
