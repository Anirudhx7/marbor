package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_NodeControlProbe_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/nodes/gpu-0/control" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu-0","configured":true,"driver":"systemd","identifier":"ollama.service","start_command":"","discovered":{"driver":"systemd","identifier":"ollama.service","evidence":["unit ollama.service found"]}}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"node", "control", "probe", "gpu-0", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"driver": "systemd"`) {
		t.Errorf("expected driver in JSON output, got %s", stdout.String())
	}
}

func TestRun_NodeControlProbe_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu-0","configured":false,"driver":"","identifier":"","start_command":"","discovered":{"driver":"","identifier":"","evidence":null}}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"node", "control", "probe", "gpu-0", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not configured") {
		t.Errorf("expected 'not configured' in table output, got %s", stdout.String())
	}
}

func TestRun_NodeControlAccept_JSON(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/nodes/gpu-0/control/accept" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu-0","configured":true,"driver":"systemd","identifier":"ollama.service"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"node", "control", "accept", "gpu-0", "--driver", "systemd", "--identifier", "ollama.service", "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if !strings.Contains(gotBody, `"driver":"systemd"`) || !strings.Contains(gotBody, `"identifier":"ollama.service"`) {
		t.Errorf("expected driver/identifier in request body, got %s", gotBody)
	}
}

func TestRun_NodeControlAccept_MissingFlags_ExitUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"node", "control", "accept", "gpu-0"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
}

func TestRun_NodeControlAccept_StartCommandPassedThrough(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"node":"gpu-0","configured":true}`))
	}))
	defer srv.Close()

	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"node", "control", "accept", "gpu-0",
		"--driver", "process", "--identifier", "/var/run/ollama.pid",
		"--start-command", "/usr/local/bin/ollama serve",
		"--server", srv.URL,
	}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if !strings.Contains(gotBody, `"start_command":"/usr/local/bin/ollama serve"`) {
		t.Errorf("expected start_command in request body, got %s", gotBody)
	}
}

func TestRun_NodeSubcommand_UnknownAction_ExitUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"node", "bogus"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d", ExitUserError, code)
	}
}
