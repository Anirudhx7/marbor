package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_NodesShowsWorkerForHeadlessWorker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"head","runtime":"vllm","health":"healthy","schedulingRole":"head"},
			{"name":"w1","runtime":"vllm","health":"down","headlessWorker":true,"schedulingRole":"worker"},
			{"name":"solo","runtime":"vllm","health":"down"}
		]`))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	// Read the HEALTH cell by the header's column offset, not by splitting on
	// spaces, so empty cells in other columns cannot shift it.
	lines := strings.Split(stdout.String(), "\n")
	col := strings.Index(lines[0], "HEALTH")
	if col < 0 {
		t.Fatalf("no HEALTH column in %q", lines[0])
	}
	health := map[string]string{}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) == 0 || len(line) <= col {
			continue
		}
		cell := strings.Fields(line[col:])
		if len(cell) > 0 {
			health[f[0]] = cell[0]
		}
	}
	if health["w1"] != healthWorker || health["solo"] != "down" || health["head"] != "healthy" {
		t.Errorf("HEALTH column = %v, want w1 worker, solo down, head healthy", health)
	}
}

func TestRun_NodesJSONKeepsRawHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"w1","health":"down","headlessWorker":true}]`))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	if got[0]["health"] != "down" || got[0]["headlessWorker"] != true {
		t.Errorf("json = %v, want raw health down and headlessWorker true", got[0])
	}
}

func TestRun_StatusWorkerSuffix(t *testing.T) {
	cases := []struct {
		name    string
		workers int
		want    string
	}{
		{"none", 0, "nodes:    3/3 healthy\n"},
		{"negative is ignored", -2, "nodes:    3/3 healthy\n"},
		{"one", 1, "nodes:    3/3 healthy, 1 worker probe failing\n"},
		{"many", 2, "nodes:    3/3 healthy, 2 workers probe failing\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := json.Marshal(map[string]any{
					"status": "ok", "version": "v", "proxy_port": 1, "uptime_seconds": 1,
					"nodes": map[string]int{"total": 3, "healthy": 3, "workers": tc.workers},
				})
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)
			withTempConfigDir(t)
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"status", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
				t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Errorf("status output = %q, want it to contain %q", stdout.String(), tc.want)
			}
			if tc.workers <= 0 && strings.Contains(stdout.String(), "probe") {
				t.Errorf("status output = %q, want no worker suffix", stdout.String())
			}
		})
	}
}

func TestRun_StatusJSONCarriesWorkers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","nodes":{"total":3,"healthy":3,"workers":2}}`))
	}))
	t.Cleanup(srv.Close)
	withTempConfigDir(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"status", "--json", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	var got HealthResp
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	if got.Nodes.Workers != 2 || got.Nodes.Total != 3 || got.Nodes.Healthy != 3 {
		t.Errorf("status --json nodes = %+v, want total 3 healthy 3 workers 2", got.Nodes)
	}
}

// The worker fields are additive. The real response structs must keep every
// existing field intact on a new payload (raw health string included), and an
// older server's payload, which lacks the new fields, must decode to the
// "no worker" values instead of failing.
func TestHeadlessWorkerFieldsAreAdditive(t *testing.T) {
	var h HealthResp
	if err := json.Unmarshal([]byte(`{"status":"ok","version":"v1","proxy_port":11434,"nodes":{"total":3,"healthy":3,"workers":2}}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Version != "v1" || h.ProxyPort != 11434 || h.Nodes.Total != 3 || h.Nodes.Healthy != 3 || h.Nodes.Workers != 2 {
		t.Errorf("new health payload decoded to %+v", h)
	}
	var oldH HealthResp
	if err := json.Unmarshal([]byte(`{"status":"ok","nodes":{"total":3,"healthy":3}}`), &oldH); err != nil || oldH.Nodes.Workers != 0 || oldH.Nodes.Total != 3 {
		t.Errorf("older health payload: %v %+v", err, oldH)
	}

	var ns []NodeResp
	if err := json.Unmarshal([]byte(`[{"name":"w1","health":"down","draining":true,"headlessWorker":true},{"name":"solo","health":"down"}]`), &ns); err != nil {
		t.Fatal(err)
	}
	if ns[0].Name != "w1" || ns[0].Health != "down" || !ns[0].Draining || !ns[0].HeadlessWorker {
		t.Errorf("new node payload decoded to %+v", ns[0])
	}
	if ns[1].Health != "down" || ns[1].HeadlessWorker {
		t.Errorf("older node payload decoded to %+v, want raw down and not a worker", ns[1])
	}
}

// An older client decodes only the fields that existed before the worker
// fields. A new payload must leave each of those fields unchanged for it.
// The payloads are literals because the server-side response structs are
// unexported in the admin package, which the cli package must not import; the
// literals mirror the wire shape, and each old field is asserted to keep its
// value.
func TestHeadlessWorkerPayloadKeepsOldFieldsForOldClients(t *testing.T) {
	type oldHealth struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		ProxyPort int    `json:"proxy_port"`
		Nodes     struct {
			Total   int `json:"total"`
			Healthy int `json:"healthy"`
		} `json:"nodes"`
	}
	type oldNode struct {
		Name     string `json:"name"`
		Health   string `json:"health"`
		Draining bool   `json:"draining"`
	}
	var h oldHealth
	if err := json.Unmarshal([]byte(`{"status":"ok","version":"v1","proxy_port":11434,"nodes":{"total":3,"healthy":3,"workers":2}}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Version != "v1" || h.ProxyPort != 11434 || h.Nodes.Total != 3 || h.Nodes.Healthy != 3 {
		t.Errorf("old health subset decoded to %+v", h)
	}
	var ns []oldNode
	if err := json.Unmarshal([]byte(`[{"name":"w1","health":"down","draining":true,"headlessWorker":true,"schedulingRole":"worker"}]`), &ns); err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || ns[0].Name != "w1" || ns[0].Health != "down" || !ns[0].Draining {
		t.Errorf("old node subset decoded to %+v", ns)
	}
}
