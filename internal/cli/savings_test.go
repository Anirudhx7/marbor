package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const savingsBody = `{"local_requests":10,"cloud_requests":2,"cloud_spent_usd":1.5,"saved_usd":9.25,"total_requests":12,"since":"2026-10-01T08:00:00Z"}`

func runSavingsCmd(t *testing.T, body string, extra ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/metrics/savings" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer tok")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	args := append([]string{"savings", "--server", srv.URL}, extra...)
	if code := Run(args, &stdout, &stderr); code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	return stdout.String()
}

func TestRun_SavingsText_LabelsAllTime(t *testing.T) {
	out := runSavingsCmd(t, savingsBody)
	if !strings.Contains(out, "(all time)") {
		t.Errorf("expected (all time) label, got %q", out)
	}
	if strings.Contains(out, "(since ") || strings.Contains(out, "2026-10-01") {
		t.Errorf("text output must not present process start as a window, got %q", out)
	}
	if !strings.Contains(out, "saved: $9.25") {
		t.Errorf("expected saved amount, got %q", out)
	}
}

func TestRun_SavingsText_NullAmountsOldServer(t *testing.T) {
	out := runSavingsCmd(t, `{"local_requests":3,"cloud_requests":1,"cloud_spent_usd":null,"saved_usd":null,"total_requests":4}`)
	for _, want := range []string{"saved: -", "cloud spent: -", "(all time)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got %q", want, out)
		}
	}
}

func TestRun_SavingsJSON_KeepsSince(t *testing.T) {
	out := runSavingsCmd(t, savingsBody, "--json")
	var got CloudSavings
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v (%q)", err, out)
	}
	if got.Since != "2026-10-01T08:00:00Z" {
		t.Errorf("--json must keep since, got %q", got.Since)
	}
}
