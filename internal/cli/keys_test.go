package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_KeySetLocalOnly(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-local-only", "finance", "true", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/keys/finance" {
		t.Errorf("expected /admin/v1/keys/finance, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"local_only":true`) {
		t.Errorf("expected body to contain local_only:true, got %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "finance") || !strings.Contains(stdout.String(), "true") {
		t.Errorf("expected confirmation mentioning finance/true, got %q", stdout.String())
	}
}

func TestRun_KeySetLocalOnly_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-local-only", "finance", "true", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v (%s)", err, stdout.String())
	}
	if out["key"] != "finance" || out["local_only"] != true || out["ok"] != true {
		t.Errorf("unexpected JSON output: %+v", out)
	}
}

func TestRun_KeySetLocalOnly_InvalidValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-local-only", "finance", "yes"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid value") {
		t.Errorf("expected an invalid-value error, got %q", stderr.String())
	}
}

func TestRun_KeySetAllowLocalDegradation(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-allow-local-degradation", "finance", "true", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/keys/finance" {
		t.Errorf("expected /admin/v1/keys/finance, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"allow_local_degradation":true`) {
		t.Errorf("expected body to contain allow_local_degradation:true, got %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "finance") || !strings.Contains(stdout.String(), "true") {
		t.Errorf("expected confirmation mentioning finance/true, got %q", stdout.String())
	}
}

func TestRun_KeySetAllowLocalDegradation_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"finance","updated":true}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-allow-local-degradation", "finance", "true", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v (%s)", err, stdout.String())
	}
	if out["key"] != "finance" || out["allow_local_degradation"] != true || out["ok"] != true {
		t.Errorf("unexpected JSON output: %+v", out)
	}
}

func TestRun_KeySetAllowLocalDegradation_InvalidValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "set-allow-local-degradation", "finance", "yes"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid value") {
		t.Errorf("expected an invalid-value error, got %q", stderr.String())
	}
}

func TestRun_Spill_JSON(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"key_name":"finance","served_by":"local","requests":5}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"spill", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/spill" {
		t.Errorf("expected /admin/v1/spill, got %s", gotPath)
	}
	if !strings.Contains(stdout.String(), "finance") {
		t.Errorf("expected finance in JSON output, got %s", stdout.String())
	}
}

func TestRun_Spill_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"key_name":"finance","served_by":"blocked","requests":2}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"spill", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "KEY") || !strings.Contains(out, "SERVED BY") || !strings.Contains(out, "REQUESTS") {
		t.Errorf("expected a table header, got %q", out)
	}
	if !strings.Contains(out, "finance") || !strings.Contains(out, "blocked") {
		t.Errorf("expected row data, got %q", out)
	}
}

func TestRun_KeyList_RateLimitCell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"open","key":"k1","rateLimit":0,"status":"active"},{"name":"capped","key":"k2","rateLimit":1000,"status":"active"}]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"key", "list", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	// Cells are whitespace-separated and the rate limit is the third column
	// (name, key preview, rate limit); the test keys have no spaces in any of
	// the first three, so positional parsing is safe here.
	cell := func(name string) string {
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n")[1:] {
			if f := strings.Fields(line); len(f) > 2 && f[0] == name {
				return f[2]
			}
		}
		t.Fatalf("no table row for key %q in %q", name, stdout.String())
		return ""
	}
	if got := cell("open"); got != "-" {
		t.Errorf("rate limit 0 should print -, got %q", got)
	}
	if got := cell("capped"); got != "1000" {
		t.Errorf("rate limit 1000 should print 1000, got %q", got)
	}

	stdout.Reset()
	if code := Run([]string{"key", "list", "--server", srv.URL, "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("json exit %d", code)
	}
	var keys []struct {
		Name      string `json:"name"`
		RateLimit int    `json:"rateLimit"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &keys); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, stdout.String())
	}
	got := map[string]int{}
	for _, k := range keys {
		got[k.Name] = k.RateLimit
	}
	if v, ok := got["open"]; !ok || v != 0 {
		t.Errorf("--json must keep rateLimit 0 on open, got %v", got)
	}
	if got["capped"] != 1000 {
		t.Errorf("--json must keep rateLimit 1000 on capped, got %v", got)
	}
}

func TestRateLimitCell(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{{0, "-"}, {-5, "-"}, {1, "1"}, {1000, "1000"}} {
		if got := rateLimitCell(c.in); got != c.want {
			t.Errorf("rateLimitCell(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
