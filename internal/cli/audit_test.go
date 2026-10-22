package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const auditPageBody = `{
	"entries": [
		{"id": 9, "time": "2026-01-01T00:00:09Z", "request_id": "req-9", "key_name": "k", "model": "m", "node": "n", "status": "200", "latency_ms": 5, "cloud": false},
		{"id": 8, "time": "2026-01-01T00:00:08Z", "request_id": "req-8", "key_name": "k", "model": "m", "node": "n", "status": "200", "latency_ms": 6, "cloud": false}
	],
	"total": 2,
	"truncated": true,
	"next_before_id": 8,
	"prev_after_id": 9
}`

// auditServer serves body for GET /admin/audit and records the last raw query
// plus how many requests arrived.
func auditServer(t *testing.T, body string) (*httptest.Server, *string, *int32) {
	t.Helper()
	var query string
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	return srv, &query, &hits
}

func TestRun_AuditSendsBeforeID(t *testing.T) {
	srv, query, _ := auditServer(t, auditPageBody)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"audit", "--before-id", "50", "--limit", "2", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !strings.Contains(*query, "before_id=50") || strings.Contains(*query, "after_id") {
		t.Errorf("query = %q, want before_id=50 only", *query)
	}
}

func TestRun_AuditSendsAfterID(t *testing.T) {
	srv, query, _ := auditServer(t, auditPageBody)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"audit", "--after-id", "7", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !strings.Contains(*query, "after_id=7") || strings.Contains(*query, "before_id") {
		t.Errorf("query = %q, want after_id=7 only", *query)
	}
}

func TestRun_AuditNoCursorSendsNoCursorParams(t *testing.T) {
	srv, query, _ := auditServer(t, auditPageBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if strings.Contains(*query, "before_id") || strings.Contains(*query, "after_id") {
		t.Errorf("query = %q, want no cursor params", *query)
	}
}

func TestRun_AuditCursorFlagMisuseIsUsageErrorWithoutRequest(t *testing.T) {
	cases := [][]string{
		{"audit", "--before-id", "5", "--after-id", "2"},
		{"audit", "--before-id=0"},
		{"audit", "--before-id=-3"},
		{"audit", "--after-id=0"},
		{"audit", "--after-id=-3"},
	}
	for _, args := range cases {
		srv, _, hits := auditServer(t, auditPageBody)
		var stdout, stderr bytes.Buffer
		code := Run(append(args, "--server", srv.URL), &stdout, &stderr)
		if code != ExitUserError {
			t.Errorf("%v: exit %d, want %d (stderr %s)", args, code, ExitUserError, stderr.String())
		}
		if n := atomic.LoadInt32(hits); n != 0 {
			t.Errorf("%v: %d requests sent, want none", args, n)
		}
		if stderr.Len() == 0 {
			t.Errorf("%v: no error message", args)
		}
	}
}

func TestRun_AuditPrintsIDColumnAndCursorHints(t *testing.T) {
	srv, _, _ := auditServer(t, auditPageBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	out := stdout.String()
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "ID") {
		t.Errorf("header does not start with ID: %q", lines[0])
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "9") {
		t.Errorf("first row does not start with id 9: %q", lines[1])
	}
	if !strings.Contains(out, "(more: --before-id 8 for older entries)") {
		t.Errorf("missing older hint in %q", out)
	}
	if !strings.Contains(out, "(newer: --after-id 9)") {
		t.Errorf("missing newer hint in %q", out)
	}
	if strings.Contains(out, "raise --limit") {
		t.Errorf("old truncated line printed alongside a cursor: %q", out)
	}
}

func TestRun_AuditHintsOmittedWhenCursorsAbsent(t *testing.T) {
	body := `{"entries":[{"id":1,"time":"2026-01-01T00:00:01Z","request_id":"r","key_name":"k","model":"m","node":"n","status":"200","latency_ms":1,"cloud":false}],"total":1,"truncated":false}`
	srv, _, _ := auditServer(t, body)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "more:") || strings.Contains(out, "newer:") || strings.Contains(out, "raise --limit") {
		t.Errorf("unexpected footer in %q", out)
	}
}

func TestRun_AuditJSONIncludesCursorFields(t *testing.T) {
	srv, _, _ := auditServer(t, auditPageBody)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout.String(), err)
	}
	if got["next_before_id"] != float64(8) || got["prev_after_id"] != float64(9) {
		t.Errorf("cursor fields missing from JSON: %v", got)
	}
	entries := got["entries"].([]any)
	if entries[0].(map[string]any)["id"] != float64(9) {
		t.Errorf("entry id missing from JSON: %v", entries[0])
	}
}

func TestRun_AuditOldServerKeepsOldTruncatedLine(t *testing.T) {
	body := `{"entries":[{"time":"2026-01-01T00:00:01Z","request_id":"r","key_name":"k","model":"m","node":"n","status":"200","latency_ms":1,"cloud":false}],"total":1,"truncated":true}`
	srv, _, _ := auditServer(t, body)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--limit", "1", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "(showing 1 of possibly more - raise --limit for older entries)") {
		t.Errorf("old truncated line missing: %q", stdout.String())
	}
}

func TestRun_AuditCursorAgainstOldServerIsAnError(t *testing.T) {
	body := `{"entries":[{"time":"2026-01-01T00:00:01Z","request_id":"r","key_name":"k","model":"m","node":"n","status":"200","latency_ms":1,"cloud":false}],"total":1,"truncated":false}`
	for _, flag := range []string{"--before-id=5", "--after-id=5"} {
		srv, _, _ := auditServer(t, body)
		var stdout, stderr bytes.Buffer
		code := Run([]string{"audit", flag, "--server", srv.URL}, &stdout, &stderr)
		if code != ExitServerError {
			t.Errorf("%s: exit %d, want %d", flag, code, ExitServerError)
		}
		if !strings.Contains(stderr.String(), flag[:strings.Index(flag, "=")]) {
			t.Errorf("%s: stderr %q does not name the flag", flag, stderr.String())
		}
		if !strings.Contains(stderr.String(), "does not support") {
			t.Errorf("%s: stderr %q lacks a does-not-support message", flag, stderr.String())
		}
	}
}

func TestRun_AuditCursorAgainstOldServerEmptyPageIsFine(t *testing.T) {
	srv, _, _ := auditServer(t, `{"entries":[],"total":0,"truncated":false}`)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--before-id=5", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
}

func TestRun_AuditCursorEmptyPagePrintsEndOfLogHint(t *testing.T) {
	srv, _, _ := auditServer(t, `{"entries":[],"total":0,"truncated":false}`)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"audit", "--after-id=5", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "(no entries: end of log)") {
		t.Errorf("stdout %q lacks the end-of-log hint", stdout.String())
	}

	srv, _, _ = auditServer(t, `{"entries":[],"total":0,"truncated":false}`)
	stdout.Reset()
	if code := Run([]string{"audit", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(stdout.String(), "end of log") {
		t.Errorf("hint printed without a cursor flag: %q", stdout.String())
	}
}
