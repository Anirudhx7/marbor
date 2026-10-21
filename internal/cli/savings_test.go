package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const savingsBody = `{"local_requests":10,"cloud_requests":2,"cloud_spent_usd":1.5,"saved_usd":9.25,"total_requests":12,"since":"2026-10-01T08:00:00Z"}`

func runSavingsCmd(t *testing.T, extra ...string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/metrics/savings" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(savingsBody))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	args := append([]string{"savings", "--server", srv.URL}, extra...)
	code := Run(args, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	return stdout.String(), code
}

func TestRun_SavingsText_LabelsAllTime(t *testing.T) {
	out, _ := runSavingsCmd(t)
	if !strings.Contains(out, "(all time)") {
		t.Errorf("expected (all time) label, got %q", out)
	}
	if strings.Contains(out, "since") || strings.Contains(out, "2026-10-01") {
		t.Errorf("text output must not present process start as a window, got %q", out)
	}
	if !strings.Contains(out, "saved: $9.25") {
		t.Errorf("expected saved amount, got %q", out)
	}
}

func TestRun_SavingsJSON_KeepsSince(t *testing.T) {
	out, _ := runSavingsCmd(t, "--json")
	if !strings.Contains(out, `"since"`) || !strings.Contains(out, "2026-10-01T08:00:00Z") {
		t.Errorf("--json must keep since, got %q", out)
	}
}
