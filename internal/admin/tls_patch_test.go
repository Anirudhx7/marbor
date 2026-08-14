package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// validFP1/validFP2 are two distinct, well-formed (but not otherwise
// meaningful) SHA-256 fingerprints for PATCH body construction, matching
// router.CertFingerprintSHA256's exact "SHA256:<64 hex chars>" output shape
// (no byte-separator colons).
var (
	validFP1 = "SHA256:" + strings.Repeat("ab", 32)
	validFP2 = "SHA256:" + strings.Repeat("cd", 32)
)

func patchNodeRequest(t *testing.T, s *Server, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name, bytes.NewReader([]byte(body)))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)
	return rec
}

// TestHandlePatchNode_SetsTLSFingerprint verifies a valid pin on an https://
// node is accepted and applied to the live node state.
func TestHandlePatchNode_SetsTLSFingerprint(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	got := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if got != validFP1 {
		t.Errorf("TLSFingerprint = %q, want %q", got, validFP1)
	}
}

// TestHandlePatchNode_RejectsInvalidTLSFingerprintFormat verifies malformed
// fingerprint values (wrong prefix, wrong length, non-hex, colon-separated
// display form) are all rejected with 400 before ever reaching PatchNode.
func TestHandlePatchNode_RejectsInvalidTLSFingerprintFormat(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	bad := []string{
		`{"tls_fingerprint":"not-a-fingerprint"}`,
		`{"tls_fingerprint":"SHA256:tooshort"}`,
		`{"tls_fingerprint":"MD5:` + strings.Repeat("a", 64) + `"}`,
		`{"tls_fingerprint":"SHA256:` + strings.Repeat("zz", 32) + `"}`,
		`{"tls_fingerprint":"SHA256:aa:bb:cc"}`,
	}
	for _, body := range bad {
		rec := patchNodeRequest(t, s, "gpu-0", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestHandlePatchNode_RejectsNoDowngrade verifies section 7: once a node has
// a pinned fingerprint, a patch that would leave it with a non-https:// URL
// must be rejected, not silently accepted.
func TestHandlePatchNode_RejectsNoDowngrade(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	rec := patchNodeRequest(t, s, "gpu-0", `{"url":"http://gpu-0:9091"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("downgrade attempt: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	url, fp := nodes[0].URL, nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if url != "https://gpu-0:9091" || fp != validFP1 {
		t.Errorf("node state after rejected downgrade: URL=%q TLSFingerprint=%q, want unchanged", url, fp)
	}
}

// TestHandlePatchNode_AllowsDowngradeWhenClearingFingerprintInSameRequest
// verifies the two-step-in-one-request escape hatch: explicitly clearing
// the pin (tls_fingerprint: "") in the SAME request that switches the URL
// to http:// is allowed - this is the intentional reset path, not a
// downgrade-attack shape.
func TestHandlePatchNode_AllowsDowngradeWhenClearingFingerprintInSameRequest(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200", rec.Code)
	}

	// tls_fingerprint:null in JSON decodes to a nil *string (Go's
	// encoding/json sets a pointer field to nil for a JSON null), which
	// means "no change" under this API's own nil-vs-set convention, not a
	// clear - so a bare "url" + null-fingerprint patch would still conflict.
	// The actual clear is an explicit empty string, in the SAME request as
	// the URL change, proving they are honored together.
	rec := patchNodeRequest(t, s, "gpu-0", `{"url":"http://gpu-0:9091","tls_fingerprint":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	url, fp := nodes[0].URL, nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if url != "http://gpu-0:9091" {
		t.Errorf("URL = %q, want http://gpu-0:9091", url)
	}
	if fp != "" {
		t.Errorf("TLSFingerprint = %q, want empty after clear", fp)
	}
}

// TestHandlePatchNode_ClearsExistingFingerprint verifies the plain reset
// path (spec section 2): an explicit empty string clears a pin while the
// URL stays https://.
func TestHandlePatchNode_ClearsExistingFingerprint(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200", rec.Code)
	}

	rec := patchNodeRequest(t, s, "gpu-0", `{"tls_fingerprint":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	fp := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if fp != "" {
		t.Errorf("TLSFingerprint = %q, want empty after clear", fp)
	}
}

// TestHandlePatchNode_RejectsSiblingFingerprintConflict verifies section 15:
// two NodeState entries sharing one physical Host (same hostname, different
// ports - a multi-GPU-per-host box) must not be allowed to carry two
// different non-empty pinned fingerprints for what is physically one Node
// Agent certificate.
func TestHandlePatchNode_RejectsSiblingFingerprintConflict(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://shared-host:11434"},
		{Name: "gpu-1", URL: "https://shared-host:11435"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	// Confirm the fixture actually produces siblings before asserting on the
	// conflict behavior that depends on it.
	nodes := r.Nodes()
	nodes[0].RLock()
	host0 := nodes[0].Host
	nodes[0].RUnlock()
	nodes[1].RLock()
	host1 := nodes[1].Host
	nodes[1].RUnlock()
	if host0 != host1 {
		t.Fatalf("fixture bug: gpu-0.Host=%q gpu-1.Host=%q, want equal (same shared-host)", host0, host1)
	}

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-0: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	rec := patchNodeRequest(t, s, "gpu-1", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP2))
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting sibling pin: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	nodes = r.Nodes()
	nodes[1].RLock()
	fp1 := nodes[1].TLSFingerprint
	nodes[1].RUnlock()
	if fp1 != "" {
		t.Errorf("gpu-1.TLSFingerprint = %q after rejected conflict, want unchanged (empty)", fp1)
	}
}

// TestHandlePatchNode_AllowsIdenticalSiblingFingerprint verifies section 15
// does not reject the correct, expected case: siblings sharing one Host
// pinned to the SAME fingerprint (which is physically accurate - one cert,
// one agent) must succeed, not be treated as a conflict.
func TestHandlePatchNode_AllowsIdenticalSiblingFingerprint(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://shared-host:11434"},
		{Name: "gpu-1", URL: "https://shared-host:11435"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-0: status = %d, want 200", rec.Code)
	}
	rec := patchNodeRequest(t, s, "gpu-1", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1))
	if rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-1 (identical fingerprint): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	for _, n := range nodes {
		n.RLock()
		fp := n.TLSFingerprint
		n.RUnlock()
		if fp != validFP1 {
			t.Errorf("node %s TLSFingerprint = %q, want %q", n.Name, fp, validFP1)
		}
	}
}

// TestHandleNodeTLSProbe_WithoutPin verifies the probe endpoint (spec
// section 2 step 2-3): it retrieves the presented certificate's fingerprint
// from a real TLS listener without pinning anything - a subsequent read of
// the node's state must still show no pin.
func TestHandleNodeTLSProbe_WithoutPin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/tls-probe", nil)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleNodeTLSProbe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(out.Fingerprint, "SHA256:") || len(out.Fingerprint) != len("SHA256:")+64 {
		t.Errorf("fingerprint = %q, want well-formed SHA256:<64 hex>", out.Fingerprint)
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	pinned := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if pinned != "" {
		t.Errorf("TLSFingerprint = %q after probe, want empty - probing must never pin", pinned)
	}
}

// TestHandleNodeTLSProbe_NeverSendsBearerToken verifies the probe never
// sends the node's bearer token (or any HTTP request at all) - it dials the
// raw TLS handshake only and reads the presented certificate, so the
// server's HTTP handler must never even be invoked. A probe that
// accidentally became a real authenticated request would leak the token to
// whatever certificate is currently presented, defeating TOFU's entire
// point (the cert hasn't been confirmed yet).
func TestHandleNodeTLSProbe_NeverSendsBearerToken(t *testing.T) {
	handlerCalled := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		if r.Header.Get("Authorization") != "" {
			t.Errorf("probe sent Authorization header %q, want none", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	}, nil)
	r.SetNodeAgent("127.0.0.1", true, mustPortForTest(t, srv.URL), "super-secret-agent-token")
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/tls-probe", nil)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleNodeTLSProbe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if handlerCalled {
		t.Error("the node's HTTP handler was invoked during a TLS probe - the probe must only perform a raw TLS handshake, never a real HTTP request")
	}
}

// mustPortForTest parses the port out of a URL for test setup - identical
// in spirit to router package's own mustPort test helper, duplicated here
// since admin's test package cannot import an unexported router test helper.
func mustPortForTest(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return port
}

// TestHandleNodeTLSProbe_RejectsNonHTTPSNode verifies the probe refuses to
// run against a plain http:// node rather than attempting a TLS handshake
// that could never succeed.
func TestHandleNodeTLSProbe_RejectsNonHTTPSNode(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/tls-probe", nil)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleNodeTLSProbe(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}
