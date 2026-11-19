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
		w.Write([]byte(`[
			{"name":"head","runtime":"vllm","health":"healthy","schedulingRole":"head"},
			{"name":"w1","runtime":"vllm","health":"down","headlessWorker":true,"schedulingRole":"worker"},
			{"name":"solo","runtime":"vllm","health":"down"}
		]`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nodes", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
	}
	health := map[string]string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			health[f[0]] = f[2]
		}
	}
	if health["w1"] != "worker" || health["solo"] != "down" || health["head"] != "healthy" {
		t.Errorf("HEALTH column = %v, want w1 worker, solo down, head healthy", health)
	}
}

func TestRun_NodesJSONKeepsRawHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"w1","health":"down","headlessWorker":true}]`))
	}))
	defer srv.Close()
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
				w.Write(body)
			}))
			defer srv.Close()
			withTempConfigDir(t)
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"status", "--server", srv.URL}, &stdout, &stderr); code != ExitOK {
				t.Fatalf("exit %d (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Errorf("status output = %q, want it to contain %q", stdout.String(), tc.want)
			}
		})
	}
}

// An older CLI (the structs as they were before the worker fields) must still
// decode a new server payload: the fields are additive.
func TestHeadlessWorkerPayloadDecodesIntoOldStructs(t *testing.T) {
	type oldHealth struct {
		Status string `json:"status"`
		Nodes  struct {
			Total   int `json:"total"`
			Healthy int `json:"healthy"`
		} `json:"nodes"`
	}
	type oldNode struct {
		Name   string `json:"name"`
		Health string `json:"health"`
	}
	var h oldHealth
	if err := json.Unmarshal([]byte(`{"status":"ok","nodes":{"total":3,"healthy":3,"workers":2}}`), &h); err != nil || h.Nodes.Total != 3 {
		t.Fatalf("old health decode: %v %+v", err, h)
	}
	var n []oldNode
	if err := json.Unmarshal([]byte(`[{"name":"w1","health":"down","headlessWorker":true}]`), &n); err != nil || n[0].Health != "down" {
		t.Fatalf("old node decode: %v %+v", err, n)
	}
}
