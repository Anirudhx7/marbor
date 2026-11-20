package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The bodies below are synthetic: written to the Admin API's documented shape.
func serveJSON(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func TestRun_RequestsListShowsCachedAndInputOrDash(t *testing.T) {
	srv := serveJSON(`[
		{"id":"r-val","time":"2026-11-20T10:00:03Z","key_name":"k","model":"m","node":"n","status":200,"latency_ms":5,"cloud":false,"cachedTokens":1820,"inputTokens":2000},
		{"id":"r-zero","time":"2026-11-20T10:00:02Z","key_name":"k","model":"m","node":"n","status":200,"latency_ms":5,"cloud":false,"cachedTokens":0,"inputTokens":1450},
		{"id":"r-none","time":"2026-11-20T10:00:01Z","key_name":"k","model":"m","node":"n","status":200,"latency_ms":5,"cloud":false}
	]`)
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"requests", "list", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines: %q", len(lines), stdout.String())
	}
	wantHeader := []string{"TIME", "ID", "KEY", "MODEL", "NODE", "STATUS", "LATENCY", "MS", "WAIT", "MS", "CLOUD", "MS", "PREFILL", "MS", "CACHED", "INPUT", "CLOUD", "REASON"}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != strings.Join(wantHeader, " ") {
		t.Errorf("header = %v, want %v", got, wantHeader)
	}
	cols := func(line string) []string { return strings.Fields(line) }
	for _, tc := range []struct {
		line, cached, input string
	}{{lines[1], "1820", "2000"}, {lines[2], "0", "1450"}, {lines[3], "-", "-"}} {
		f := cols(tc.line)
		// ... PREFILL(-) CACHED INPUT CLOUD(no) REASON(empty): the last fields
		// are cached, input, "no".
		n := len(f)
		if f[n-3] != tc.cached || f[n-2] != tc.input {
			t.Errorf("row %q: cached/input = %q/%q, want %q/%q", tc.line, f[n-3], f[n-2], tc.cached, tc.input)
		}
	}
}

func TestRun_RequestsListJSONCarriesCacheFields(t *testing.T) {
	srv := serveJSON(`[{"id":"r","time":"2026-11-20T10:00:03Z","key_name":"k","model":"m","node":"n","status":200,"latency_ms":5,"cloud":false,"cachedTokens":7,"inputTokens":9}]`)
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"requests", "list", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, `"cachedTokens": 7`) || !strings.Contains(out, `"inputTokens": 9`) {
		t.Errorf("json output lacks cache fields: %s", out)
	}
}

func TestRun_AnalyticsShowsCacheWindowRaw(t *testing.T) {
	srv := serveJSON(`{"hourly":[],"by_model":[],"cache_window":{"window_size":1000,"window_requests":3,"reporting_requests":1,"window_oldest":null,"by_model":[],"by_key":[]}}`)
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"analytics", "show", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var out struct {
		CacheWindow struct {
			WindowSize        int `json:"window_size"`
			WindowRequests    int `json:"window_requests"`
			ReportingRequests int `json:"reporting_requests"`
		} `json:"cache_window"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("analytics output is not JSON: %v\n%s", err, stdout.String())
	}
	if cw := out.CacheWindow; cw.WindowSize != 1000 || cw.WindowRequests != 3 || cw.ReportingRequests != 1 {
		t.Errorf("cache_window = %+v, want 1000/3/1", cw)
	}
}

func TestRun_AuditListShowsCachedAndInputOrDash(t *testing.T) {
	srv := serveJSON(`{"entries":[
		{"id":2,"time":"2026-11-20T10:00:03Z","request_id":"r-val","key_name":"k","model":"m","node":"n","status":"200","latency_ms":5,"cloud":false,"cachedTokens":1820,"inputTokens":2000},
		{"id":1,"time":"2026-11-20T10:00:02Z","request_id":"r-none","key_name":"k","model":"m","node":"n","status":"200","latency_ms":5,"cloud":false}
	],"total":2,"truncated":false}`)
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("output: %q", stdout.String())
	}
	if !strings.Contains(lines[0], "CACHED") || !strings.Contains(lines[0], "INPUT") {
		t.Errorf("header lacks CACHED/INPUT: %q", lines[0])
	}
	for i, want := range [][2]string{{"1820", "2000"}, {"-", "-"}} {
		f := strings.Fields(lines[i+1])
		n := len(f)
		if f[n-3] != want[0] || f[n-2] != want[1] {
			t.Errorf("row %q: cached/input = %q/%q, want %q/%q", lines[i+1], f[n-3], f[n-2], want[0], want[1])
		}
	}
}
