package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// nodesFitServer answers GET /admin/nodes/model-fit with body.
func nodesFitServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/nodes/model-fit" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	return srv
}

func runNodesFitCmd(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	argv := append([]string{"nodes", "fit"}, args...)
	code := Run(append(argv, "--server", srv.URL), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const nodesFitBody = `{"future_top":7,"nodes":[{"name":"gpu-a","url":"http://a:11434","vram_free_bytes":8589934592,"vram_total_bytes":17179869184,"vram_source":"nvidia-smi","future_node":"x","models":[{"name":"m1","size_bytes":1,"vram_estimate_bytes":2,"fit":"green","loaded":true}]}]}`

func TestRun_NodesFit_JSONPrintsEnvelope(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, nodesFitBody), "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	var nodes []map[string]any
	if err := json.Unmarshal(got["nodes"], &nodes); err != nil || len(nodes) != 1 || nodes[0]["name"] != "gpu-a" {
		t.Fatalf("nodes array wrong: %v %s", err, got["nodes"])
	}
}

func TestRun_NodesFit_JSONKeepsUnknownFields(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, nodesFitBody), "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	if string(got["future_top"]) != "7" {
		t.Errorf("top-level future_top dropped: %s", out)
	}
	var nodes []map[string]any
	if err := json.Unmarshal(got["nodes"], &nodes); err != nil || len(nodes) != 1 || nodes[0]["future_node"] != "x" {
		t.Errorf("future_node dropped: %v %s", err, got["nodes"])
	}
}

func TestRun_NodesFit_JSONEmptyStaysObject(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, `{"nodes":[]}`), "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var got map[string][]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["nodes"] == nil || len(got["nodes"]) != 0 {
		t.Errorf("want {\"nodes\":[]}, got %q (%v)", out, err)
	}
}

func TestRun_NodesFit_JSONNullNodesPrintedAsSent(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, `{"nodes":null}`), "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil || string(got["nodes"]) != "null" {
		t.Errorf("want nodes null as sent, got %q (%v)", out, err)
	}
}

func TestRun_NodesFit_TableHeaderUnchanged(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, nodesFitBody))
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header and one row, got:\n%s", out)
	}
	if !strings.HasPrefix(lines[0], "NODE") || !strings.Contains(lines[0], "VRAM FREE/TOTAL") || !strings.HasSuffix(lines[0], "LOADED") {
		t.Errorf("header changed: %q", lines[0])
	}
	f := strings.Fields(lines[1])
	if len(f) < 5 || f[0] != "gpu-a" || strings.Join(f[len(f)-4:], " ") != "nvidia-smi m1 green true" {
		t.Errorf("row wrong: %q", lines[1])
	}
}

func TestRun_NodesFit_TableNullNodesPrintsHeaderOnly(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, `{"nodes":null}`))
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 || !strings.HasPrefix(out, "NODE") {
		t.Errorf("want header line only, got %q", out)
	}
}

func TestRun_NodesFit_TableWrongTypedNodesIsServerError(t *testing.T) {
	for _, body := range []string{`{"nodes":{}}`, `{"nodes":"x"}`} {
		code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, body))
		if code != ExitServerError || out != "" || !strings.Contains(errOut, "/admin/nodes/model-fit") {
			t.Errorf("body %q: code=%d stdout=%q stderr=%q", body, code, out, errOut)
		}
	}
}

func TestRun_NodesFit_JSONRejectsBodyWithoutNodes(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[]`, ``, `{"error":"x"}`, `not json`} {
		code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, body), "--json")
		if code != ExitServerError || out != "" || !strings.Contains(errOut, "/admin/nodes/model-fit") {
			t.Errorf("body %q: code=%d stdout=%q stderr=%q, want server error, empty stdout", body, code, out, errOut)
		}
	}
}

func TestRun_NodesFit_UndecodableBodyWithoutJSONFlag(t *testing.T) {
	code, out, errOut := runNodesFitCmd(t, nodesFitServer(t, `not json`))
	if code != ExitServerError || out != "" || !strings.Contains(errOut, "could not parse model fit response") {
		t.Errorf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestRun_NodesFit_HTTPErrorsWithJSON(t *testing.T) {
	want := map[int]int{http.StatusUnauthorized: ExitAuthError, http.StatusInternalServerError: ExitServerError}
	for status := range want {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", status)
		}))
		withTempConfigDir(t)
		mustSaveSession(t, srv.URL, "tok")
		code, out, errOut := runNodesFitCmd(t, srv, "--json")
		srv.Close()
		if code != want[status] || out != "" || !strings.Contains(errOut, "nope") {
			t.Errorf("status %d: code=%d stdout=%q stderr=%q, want exit %d, empty stdout and the server message on stderr", status, code, out, errOut, want[status])
		}
	}
}
