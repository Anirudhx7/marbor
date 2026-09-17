package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_RequestsExplain(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"node": "gpu-0",
			"reason": "score_based",
			"detail": "score_based on node gpu-0",
			"score": 42.5,
			"components": [
				{"name": "warm_model_resident", "raw": 0, "weight": 50, "value": 0},
				{"name": "free_vram_headroom", "raw": 1, "weight": 20, "value": 20}
			]
		}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"requests", "explain", "req-1", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/requests/req-1/explain" {
		t.Errorf("expected /admin/v1/requests/req-1/explain, got %s", gotPath)
	}
	out := stdout.String()
	if !strings.Contains(out, "gpu-0") || !strings.Contains(out, "score_based") {
		t.Errorf("expected output to mention node and reason, got %q", out)
	}
	if !strings.Contains(out, "free_vram_headroom") {
		t.Errorf("expected output to include the component breakdown, got %q", out)
	}
}

func TestRun_RequestsExplain_ExcludedCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"node": "gpu-0",
			"reason": "score_based",
			"score": 42.5,
			"components": [
				{"name": "warm_model_resident", "raw": 0, "weight": 50, "value": 0, "phase": "locality"}
			],
			"excluded": [
				{"node": "gpu-1", "reason": "unhealthy"},
				{"node": "gpu-2", "reason": "over_capacity"}
			],
			"excludedTotal": 5
		}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"requests", "explain", "req-1", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "EXCLUDED") {
		t.Errorf("expected an EXCLUDED section, got %q", out)
	}
	if !strings.Contains(out, "gpu-1") || !strings.Contains(out, "node is unhealthy") {
		t.Errorf("expected gpu-1's translated reason, got %q", out)
	}
	if !strings.Contains(out, "gpu-2") || !strings.Contains(out, "over the per-node request cap") {
		t.Errorf("expected gpu-2's translated reason, got %q", out)
	}
	if !strings.Contains(out, "...and 3 more") {
		t.Errorf("expected truncation note for excludedTotal(5) - len(excluded)(2) = 3, got %q", out)
	}
}

func TestRun_RequestsExplain_MissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"requests", "explain"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage: marbor requests explain") {
		t.Errorf("expected a usage error, got %q", stderr.String())
	}
}
