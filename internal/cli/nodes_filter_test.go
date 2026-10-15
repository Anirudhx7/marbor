package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func parseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query %q: %v", raw, err)
	}
	return q
}

// nodesFilterServer serves /admin/v1/nodes with the given status and body and
// records the raw query of every request.
func nodesFilterServer(t *testing.T, status int, body string, queries *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	return srv
}

const twoNodesWithSignals = `[
	{"name":"gpu-0","host":"h0","port":11434,"runtime":"ollama","health":"degraded","gpuModel":"A100","signals":["degraded"]},
	{"name":"gpu-1","host":"h1","port":11434,"runtime":"vllm","health":"down","gpuModel":"H100","signals":["down"]}
]`

func TestRun_NodesFlagsBecomeQueryParameters(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantSignal string
		wantQ      string
	}{
		{"comma list", []string{"--signal", "degraded,down"}, "degraded,down", ""},
		{"repeated flag", []string{"--signal", "degraded", "--signal", "down"}, "degraded,down", ""},
		{"comma and repeated", []string{"--signal=degraded,warm", "--signal=pressure"}, "degraded,warm,pressure", ""},
		{"search only", []string{"--search", "a100"}, "", "a100"},
		{"both", []string{"--signal", "down", "--search", "gpu 1"}, "down", "gpu 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var queries []string
			srv := nodesFilterServer(t, http.StatusOK, twoNodesWithSignals, &queries)
			var stdout, stderr bytes.Buffer
			args := append([]string{"nodes"}, tt.args...)
			args = append(args, "--server", srv.URL)
			if code := Run(args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
			}
			if len(queries) != 1 {
				t.Fatalf("got %d requests, want 1", len(queries))
			}
			q := parseQuery(t, queries[0])
			if got := q.Get("signal"); got != tt.wantSignal {
				t.Errorf("signal = %q, want %q", got, tt.wantSignal)
			}
			if got := q.Get("q"); got != tt.wantQ {
				t.Errorf("q = %q, want %q", got, tt.wantQ)
			}
		})
	}
}

func TestRun_NodesWithoutFlagsSendsNoQuery(t *testing.T) {
	var queries []string
	srv := nodesFilterServer(t, http.StatusOK, `[{"name":"gpu-0","health":"healthy"}]`, &queries)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	if len(queries) != 1 || queries[0] != "" {
		t.Fatalf("queries = %q, want one request with no query", queries)
	}
}

// An older server has no signals field and ignores the filter parameters, so
// a filtered request would silently return every node. The CLI must refuse.
func TestRun_NodesFilterAgainstOldServerFailsClearly(t *testing.T) {
	old := `[{"name":"gpu-0","health":"healthy"},{"name":"gpu-1","health":"down"}]`
	for _, flagArgs := range [][]string{{"--signal", "down"}, {"--search", "gpu"}} {
		var queries []string
		srv := nodesFilterServer(t, http.StatusOK, old, &queries)
		var stdout, stderr bytes.Buffer
		args := append([]string{"nodes"}, flagArgs...)
		args = append(args, "--server", srv.URL)
		code := Run(args, &stdout, &stderr)
		if code == ExitOK {
			t.Fatalf("%v: exit 0 against a server without signals; stdout: %s", flagArgs, stdout.String())
		}
		if !strings.Contains(stderr.String(), "too old") || !strings.Contains(stderr.String(), "upgrade") {
			t.Errorf("%v: stderr = %q, want a server-too-old message", flagArgs, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: printed an unfiltered list: %s", flagArgs, stdout.String())
		}
	}
}

func TestRun_NodesUnfilteredAgainstOldServerStillWorks(t *testing.T) {
	var queries []string
	srv := nodesFilterServer(t, http.StatusOK, `[{"name":"gpu-0","health":"healthy"}]`, &queries)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gpu-0") {
		t.Errorf("missing node row: %s", stdout.String())
	}
}

func TestRun_NodesServerRejectsUnknownSignal(t *testing.T) {
	var queries []string
	msg := `unknown signal "bogus": valid signals are healthy, degraded, draining, down, agent, warm, pressure`
	body, _ := json.Marshal(map[string]string{"error": msg})
	srv := nodesFilterServer(t, http.StatusBadRequest, string(body), &queries)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--signal", "bogus", "--server", srv.URL}, &stdout, &stderr)
	if code == ExitOK {
		t.Fatal("exit 0, want a non-zero exit for a rejected signal")
	}
	if !strings.Contains(stderr.String(), msg) {
		t.Errorf("stderr = %q, want the server's message %q", stderr.String(), msg)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRun_NodesEmptyMatchIsHeaderOnlyAndExitsZero(t *testing.T) {
	var queries []string
	srv := nodesFilterServer(t, http.StatusOK, `[]`, &queries)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--signal", "down", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "NAME") {
		t.Errorf("stdout = %q, want only the header row", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"nodes", "--signal", "down", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("json exit %d (stderr: %s)", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("json stdout = %q, want []", stdout.String())
	}
}

func TestRun_NodesJSONCarriesSignals(t *testing.T) {
	var queries []string
	srv := nodesFilterServer(t, http.StatusOK, twoNodesWithSignals, &queries)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--signal", "degraded,down", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	var got []struct {
		Name    string   `json:"name"`
		Signals []string `json:"signals"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", stdout.String(), err)
	}
	if len(got) != 2 || got[0].Signals[0] != "degraded" || got[1].Signals[0] != "down" {
		t.Errorf("got %+v", got)
	}
}

// With no filter flags the table is exactly what it was before filtering
// existed, whether or not the server sends signals.
func TestRun_NodesTableUnchangedWithoutFlags(t *testing.T) {
	base := `{"name":"gpu-0","host":"h0","port":11434,"runtime":"ollama","health":"healthy","gpuModel":"A100","vramTotalMB":81920,"vramUsedMB":1024,"loadedModels":[{"name":"m"}],"draining":false`
	withSignals := `[` + base + `,"signals":["healthy","warm"]}]`
	without := `[` + base + `}]`

	run := func(body string) string {
		var queries []string
		srv := nodesFilterServer(t, http.StatusOK, body, &queries)
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"nodes", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
			t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
		}
		return stdout.String()
	}
	a, b := run(withSignals), run(without)
	if a != b {
		t.Fatalf("table differs when the server sends signals:\n%s\nvs\n%s", a, b)
	}
	want := "NAME   HOST:PORT  HEALTH   RUNTIME  GPU   VRAM USED/TOTAL     MODELS WARM  DRAINING  REPLICA ROLE  REPLICA HEAD  DETECTED GPUS  KEEP-WARM\n" +
		"gpu-0  h0:11434   healthy  ollama   A100  1024 MB / 81920 MB  1            no        standalone    -             -              -\n"
	if a != want {
		t.Errorf("table output changed:\n got: %q\nwant: %q", a, want)
	}
}
