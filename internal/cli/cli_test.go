package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"bogus"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d", ExitUserError, code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("expected usage error in stderr, got %q", stderr.String())
	}
}

func TestRun_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--help"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d", ExitOK, code)
	}
	if !strings.Contains(stdout.String(), "ollama-mesh - CLI client") {
		t.Fatalf("expected usage text in stdout, got %q", stdout.String())
	}
}

func TestRun_UnknownFlag_WritesToInjectedStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--bogusflag"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d", ExitUserError, code)
	}
	if stderr.Len() == 0 {
		t.Fatal("expected the flag-parse error to be written to the injected stderr writer, got an empty buffer")
	}
}

func TestRun_Version_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","version":"v0.19.0","nodes":{"total":0,"healthy":0}}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"version", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out versionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("could not parse JSON output: %v (%s)", err, stdout.String())
	}
	if !out.ServerReachable || out.ServerVersion != "v0.19.0" {
		t.Fatalf("unexpected version output: %+v", out)
	}
}

func TestRun_Version_ServerUnreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version", "--server", "http://127.0.0.1:1", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("version must not fail just because the server is unreachable, got exit %d", code)
	}
	var out versionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("could not parse JSON output: %v", err)
	}
	if out.ServerReachable {
		t.Fatalf("expected server_reachable=false, got %+v", out)
	}
}

func TestRun_Version_JSON_AlwaysHasServerVersionKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A reachable server with no version stamped (e.g. a local dev build
		// with no -X ldflag) reports version:"".
		w.Write([]byte(`{"status":"ok","version":"","nodes":{"total":0,"healthy":0}}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"version", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("could not parse JSON output: %v", err)
	}
	if _, ok := raw["server_version"]; !ok {
		t.Fatalf("expected server_version key to always be present in the --json contract, got %v", raw)
	}
}

func TestRun_Status_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","version":"v0.19.0","proxy_port":11434,"uptime_seconds":99,"nodes":{"total":3,"healthy":3}}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var health HealthResp
	if err := json.Unmarshal(stdout.Bytes(), &health); err != nil {
		t.Fatalf("could not parse JSON output: %v", err)
	}
	if health.Nodes.Total != 3 || health.ProxyPort != 11434 {
		t.Fatalf("--json output did not round-trip the server payload: %+v", health)
	}
}

func TestRun_Status_ServerDown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", "--server", "http://127.0.0.1:1"}, &stdout, &stderr)
	if code != ExitServerError {
		t.Fatalf("expected exit %d for an unreachable server, got %d", ExitServerError, code)
	}
}

func TestRun_Nodes_NoCredentials(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", "http://example.invalid"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d for missing credentials, got %d", ExitUserError, code)
	}
}

func TestRun_Nodes_WithToken_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/nodes" && r.Header.Get("Authorization") == "Bearer good-token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"name":"gpu-01","host":"10.0.0.1","port":11434,"health":"healthy","runtime":"ollama","gpuModel":"RTX 4090","vramTotalMB":24576,"vramUsedMB":1024,"draining":false,"loadedModels":[]}]`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL, "--token", "good-token", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var nodes []NodeResp
	if err := json.Unmarshal(stdout.Bytes(), &nodes); err != nil {
		t.Fatalf("could not parse JSON output: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "gpu-01" {
		t.Fatalf("unexpected nodes output: %+v", nodes)
	}
}

func TestRun_Nodes_LoginFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "mesh_session", Value: "session-abc"})
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"role":"admin","username":"admin"}`))
		case "/admin/v1/nodes":
			if r.Header.Get("Authorization") != "Bearer session-abc" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL, "--username", "admin", "--password", "admin", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
}

func TestRun_Nodes_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "--server", srv.URL, "--token", "bad"}, &stdout, &stderr)
	if code != ExitAuthError {
		t.Fatalf("expected exit %d, got %d", ExitAuthError, code)
	}
}

func TestRun_Models_WithToken_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/models" && r.Header.Get("Authorization") == "Bearer good-token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"models":[{"name":"llama3","size_vram":1048576,"warm_count":1,"total_nodes":1}],"total_models":1,"total_nodes":1,"healthy_nodes":1}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "--server", srv.URL, "--token", "good-token", "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var models ModelsResp
	if err := json.Unmarshal(stdout.Bytes(), &models); err != nil {
		t.Fatalf("could not parse JSON output: %v", err)
	}
	if models.TotalModels != 1 || models.Models[0].Name != "llama3" {
		t.Fatalf("unexpected models output: %+v", models)
	}
}

func TestRun_Models_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3","size_vram":1048576,"warm_count":1,"total_nodes":2,"family":"llama"}],"total_models":1,"total_nodes":2,"healthy_nodes":2}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "--server", srv.URL, "--token", "abc"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "llama3") {
		t.Fatalf("expected table output to contain model name, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "HEALTHY NODES") {
		t.Fatalf("expected the column header to say HEALTHY (not TOTAL) since the server field is a healthy-node count, got %q", stdout.String())
	}
}
