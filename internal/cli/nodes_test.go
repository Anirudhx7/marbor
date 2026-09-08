package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testValidFingerprint = "SHA256:" + "ab" + "cd" + "ef" + "01" + "23" + "45" + "67" + "89" +
	"ab" + "cd" + "ef" + "01" + "23" + "45" + "67" + "89" +
	"ab" + "cd" + "ef" + "01" + "23" + "45" + "67" + "89" +
	"ab" + "cd" + "ef" + "01" + "23" + "45" + "67" + "89"

func TestRun_NodesConfirmTLS(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"gpu-0","tlsFingerprint":"` + testValidFingerprint + `"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "confirm-tls", "gpu-0", "--fingerprint=" + testValidFingerprint, "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/admin/v1/nodes/gpu-0" {
		t.Errorf("expected /admin/v1/nodes/gpu-0, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"tls_fingerprint":"`+testValidFingerprint+`"`) {
		t.Errorf("expected body to contain tls_fingerprint, got %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "gpu-0") || !strings.Contains(stdout.String(), testValidFingerprint) {
		t.Errorf("expected confirmation mentioning node name and fingerprint, got %q", stdout.String())
	}
}

func TestRun_NodesConfirmTLS_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"gpu-0","tlsFingerprint":"` + testValidFingerprint + `"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "confirm-tls", "gpu-0", "--fingerprint=" + testValidFingerprint, "--server", srv.URL, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v (%s)", err, stdout.String())
	}
	if out["node"] != "gpu-0" || out["tls_fingerprint"] != testValidFingerprint || out["ok"] != true {
		t.Errorf("unexpected JSON output: %+v", out)
	}
}

func TestRun_NodesConfirmTLS_MissingFingerprint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "confirm-tls", "gpu-0"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--fingerprint is required") {
		t.Errorf("expected a required-fingerprint error, got %q", stderr.String())
	}
}

func TestRun_NodesConfirmTLS_InvalidFingerprintFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "confirm-tls", "gpu-0", "--fingerprint=not-a-fingerprint"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid --fingerprint") {
		t.Errorf("expected an invalid-fingerprint error, got %q", stderr.String())
	}
}

func TestRun_NodesConfirmTLS_MissingNodeName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "confirm-tls", "--fingerprint=" + testValidFingerprint}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage: marbor nodes confirm-tls") {
		t.Errorf("expected a usage error, got %q", stderr.String())
	}
}

func TestRun_Nodes_UnknownAction(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "delete", "gpu-0"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown nodes action") {
		t.Errorf("expected an unknown-action error, got %q", stderr.String())
	}
}

// patchNodeTestServer records the decoded PATCH /admin/v1/nodes/{name} body
// sent by "nodes patch" and returns a minimal 200 OK, mirroring
// TestRun_KeyPatch_SendsOnlyVisited's send-body assertion pattern.
func patchNodeTestServer(t *testing.T) (*httptest.Server, *map[string]interface{}) {
	t.Helper()
	gotBody := map[string]interface{}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"gpu-0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody
}

func TestRun_NodesPatch_URL(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--url", "http://10.0.0.5:11434", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if (*gotBody)["url"] != "http://10.0.0.5:11434" {
		t.Errorf("expected url in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_Runtime(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--runtime", "vllm", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if (*gotBody)["runtime"] != "vllm" {
		t.Errorf("expected runtime in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_GPUModelClear(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--gpu-model", "", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if val, ok := (*gotBody)["gpu_model"]; !ok || val != "" {
		t.Errorf("expected gpu_model=\"\" in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_VRAMTotalMB(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--vram-total-mb", "24000", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if (*gotBody)["vram_total_mb"] != float64(24000) {
		t.Errorf("expected vram_total_mb=24000 in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_GPUIndices(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--gpu-indices", "0,1", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	indices, ok := (*gotBody)["gpu_indices"].([]interface{})
	if !ok || len(indices) != 2 || indices[0] != float64(0) || indices[1] != float64(1) {
		t.Errorf("expected gpu_indices=[0,1] in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_GPUIndices_InvalidEntry(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--gpu-indices", "0,notanumber", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid --gpu-indices") {
		t.Errorf("expected invalid --gpu-indices error, got %q", stderr.String())
	}
}

func TestRun_NodesPatch_MaxInFlight(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--max-in-flight", "10", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if (*gotBody)["max_in_flight"] != float64(10) {
		t.Errorf("expected max_in_flight=10 in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_MaxInFlightClear(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--max-in-flight", "0", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if val, ok := (*gotBody)["max_in_flight"]; !ok || val != float64(0) {
		t.Errorf("expected max_in_flight=0 in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_TLSClear(t *testing.T) {
	srv, gotBody := patchNodeTestServer(t)
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--tls-clear", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("got %d %q", code, stderr.String())
	}
	if val, ok := (*gotBody)["tls_fingerprint"]; !ok || val != "" {
		t.Errorf("expected tls_fingerprint=\"\" in body, got %v", *gotBody)
	}
}

func TestRun_NodesPatch_EmptyURL_Rejected(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--url", "", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--url cannot be empty") {
		t.Errorf("expected --url cannot be empty error, got %q", stderr.String())
	}
}

func TestRun_NodesPatch_EmptyRuntime_Rejected(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--runtime", "", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--runtime cannot be empty") {
		t.Errorf("expected --runtime cannot be empty error, got %q", stderr.String())
	}
}

func TestRun_NodesPatch_NoFlags_Rejected(t *testing.T) {
	withTempConfigDir(t)
	mustSaveSession(t, "http://x", "tok")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "patch", "gpu-0", "--server", "http://x"}, &stdout, &stderr)
	if code != ExitUserError {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitUserError, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "at least one field flag is required") {
		t.Errorf("expected a field-required error, got %q", stderr.String())
	}
}

func TestRun_NodesTLSProbe(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"fingerprint":"` + testValidFingerprint + `"}`))
	}))
	defer srv.Close()
	withTempConfigDir(t)
	mustSaveSession(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"nodes", "tls-probe", "gpu-0", "--server", srv.URL}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr: %s)", ExitOK, code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/admin/nodes/gpu-0/tls-probe" {
		t.Errorf("expected /admin/nodes/gpu-0/tls-probe, got %s", gotPath)
	}
	if !strings.Contains(stdout.String(), testValidFingerprint) {
		t.Errorf("expected fingerprint in output, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "NOT pinned") {
		t.Errorf("expected a not-pinned disclaimer, got %q", stdout.String())
	}
}
