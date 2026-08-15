package cli

import (
	"bytes"
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
	if !strings.Contains(stderr.String(), "usage: ollama-mesh nodes confirm-tls") {
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
