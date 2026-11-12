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
	_, out, _ := runNodesFitCmd(t, nodesFitServer(t, nodesFitBody), "--json")
	for _, want := range []string{`"future_top"`, `"future_node"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output dropped %s:\n%s", want, out)
		}
	}
}

func TestRun_NodesFit_JSONEmptyStaysObject(t *testing.T) {
	_, out, _ := runNodesFitCmd(t, nodesFitServer(t, `{"nodes":[]}`), "--json")
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
	if !strings.HasPrefix(out, "NODE") || !strings.Contains(out, "VRAM FREE/TOTAL") || !strings.Contains(out, "LOADED") || !strings.Contains(out, "gpu-a") {
		t.Errorf("table changed:\n%s", out)
	}
}

func TestRun_NodesFit_UndecodableBodyIsServerError(t *testing.T) {
	code, _, errOut := runNodesFitCmd(t, nodesFitServer(t, `not json`), "--json")
	if code != ExitServerError || !strings.Contains(errOut, "could not parse model fit response") {
		t.Errorf("code=%d stderr=%s, want server error", code, errOut)
	}
}
