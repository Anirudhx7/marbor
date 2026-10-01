package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fitCatalogJSON = `{"nodes":[
 {"name":"gpu-a","models":[
  {"name":"llama3.1:8b","downloaded":true,"recommendation":{"picked":true,"tag":"llama3.1:8b","quantization":"Q4_K_M","vram_est_mb":4813,"fit":"green"}},
  {"name":"mixtral:8x7b","recommendation":{"picked":true,"tag":"mixtral:8x7b","quantization":"Q4_K_M","vram_est_mb":26624,"fit":"yellow","tight":true}},
  {"name":"llama3.1:70b","recommendation":{"picked":false,"reason":"too_large","closest_tag":"llama3.1:70b"}}]},
 {"name":"gpu-b","models":[
  {"name":"llama3.1:8b","recommendation":{"picked":false,"reason":"incompatible_runtime"}},
  {"name":"phi4:14b","recommendation":{"picked":false,"reason":"vram_unknown"}}]}]}`

func fitServer(t *testing.T, repoQueries *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/models/catalog":
			w.Write([]byte(fitCatalogJSON))
		case "/admin/models/repo":
			*repoQueries = append(*repoQueries, r.URL.RawQuery)
			w.Write([]byte(`{"recommendation":{"picked":true,"tag":"hf.co/o/m:Q4_K_M","quantization":"Q4_K_M","vram_est_mb":5000,"fit":"green"},"variants":[{"downloaded":true}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	return srv
}

func runFitCmd(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(append(append([]string{"fit"}, args...), "--server", srv.URL), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRun_Fit_CatalogTable(t *testing.T) {
	var q []string
	code, out, errOut := runFitCmd(t, fitServer(t, &q))
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	for _, want := range []string{
		"llama3.1:8b (Q4_K_M)", "~4813 MB", "a version is downloaded",
		"tight", "tight fit: uses most of the VRAM",
		"too large for this node; closest: llama3.1:70b",
		"other format for this node's runtime", "VRAM or model size unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRun_Fit_NodeFilterAndUnknownNode(t *testing.T) {
	var q []string
	srv := fitServer(t, &q)
	code, out, _ := runFitCmd(t, srv, "--node", "gpu-b")
	if code != ExitOK || strings.Contains(out, "gpu-a") || !strings.Contains(out, "gpu-b") {
		t.Errorf("--node gpu-b: code=%d out=%s", code, out)
	}
	code, _, errOut := runFitCmd(t, srv, "--node", "nope")
	if code != ExitUserError || !strings.Contains(errOut, "gpu-a, gpu-b") {
		t.Errorf("unknown node: code=%d stderr=%s, want user error listing valid nodes", code, errOut)
	}
}

func TestRun_Fit_JSON(t *testing.T) {
	var q []string
	code, out, errOut := runFitCmd(t, fitServer(t, &q), "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var rows []fitRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 5 {
		t.Fatalf("json rows = %d, err = %v:\n%s", len(rows), err, out)
	}
}

func TestRun_Fit_RepoMode(t *testing.T) {
	var q []string
	srv := fitServer(t, &q)
	code, out, errOut := runFitCmd(t, srv, "o/m", "--ctx", "4096")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if len(q) != 2 || !strings.Contains(q[0], "node=gpu-a") || !strings.Contains(q[1], "node=gpu-b") || !strings.Contains(q[0], "ctx=4096") {
		t.Errorf("repo queries = %v, want one per node carrying ctx", q)
	}
	if !strings.Contains(out, "hf.co/o/m:Q4_K_M (Q4_K_M)") || !strings.Contains(out, "REPO") {
		t.Errorf("repo output:\n%s", out)
	}
	q = nil
	if code, _, _ := runFitCmd(t, srv, "o/m", "--node", "gpu-b", "--json"); code != ExitOK || len(q) != 1 {
		t.Errorf("repo --node --json: code=%d queries=%v", code, q)
	}
}

func TestRun_Fit_CtxValidation(t *testing.T) {
	var q []string
	srv := fitServer(t, &q)
	if code, _, errOut := runFitCmd(t, srv, "--ctx", "4096"); code != ExitUserError || !strings.Contains(errOut, "owner/name") {
		t.Errorf("--ctx without repo: code=%d stderr=%s", code, errOut)
	}
	if code, _, errOut := runFitCmd(t, srv, "o/m", "--ctx", "-5"); code != ExitUserError || !strings.Contains(errOut, "positive") {
		t.Errorf("negative --ctx: code=%d stderr=%s", code, errOut)
	}
}

func TestFitNote_Branches(t *testing.T) {
	cases := map[string]fitRow{
		"not enough free disk":     {Recommendation: fitPick{Reason: "disk_insufficient"}},
		"no quantizations offered": {Recommendation: fitPick{Reason: "no_variants"}},
		"too large for this node":  {Recommendation: fitPick{Reason: "too_large"}},
		"-":                        {Recommendation: fitPick{Reason: "something_new"}},
		"a version is downloaded":  {Downloaded: true, Recommendation: fitPick{Picked: true}},
		"VRAM or model size unknown; a version is downloaded": {Downloaded: true, Recommendation: fitPick{Reason: "vram_unknown"}},
	}
	for want, row := range cases {
		if got := fitNote(row); got != want {
			t.Errorf("fitNote(%+v) = %q, want %q", row.Recommendation, got, want)
		}
	}
	if got := fitPickCell(fitPick{Picked: true, Tag: "x"}); got != "x" {
		t.Errorf("pick cell without quantization = %q, want x", got)
	}
}
