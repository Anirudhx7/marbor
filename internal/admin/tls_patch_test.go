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
	"sync"
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
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
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

// TestHandlePatchNode_URLSchemeAloneDoesNotAffectAgentPin verifies the core
// behavior this fix's decoupling was built for: section 7's no-downgrade
// rule is keyed off the Node Agent's OWN scheme for the resulting host, not
// the node's runtime URL scheme. Before this fix, the pinned fingerprint was
// (incorrectly) tied to the runtime URL's own https-ness, so flipping the
// URL to http:// while pinned was rejected as a "downgrade" - even though
// the pinned certificate belongs to the Agent, which is unaffected by the
// runtime URL at all. Since the Agent here stays configured for https:// on
// this same host throughout, changing the runtime URL's scheme alone must be
// allowed and must not touch the existing pin. (A move to a host whose Agent
// is NOT https-configured is still rejected - see
// TestHandlePatchNode_RejectsURLOnlyMoveOntoConflictingSiblingHost and
// TestHandlePatchNode_PinGatingIgnoresCurrentHostAgentScheme.)
func TestHandlePatchNode_URLSchemeAloneDoesNotAffectAgentPin(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	rec := patchNodeRequest(t, s, "gpu-0", `{"url":"http://gpu-0:9091"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("runtime-URL-only scheme change (Agent stays https-configured): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	url, fp := nodes[0].URL, nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if url != "http://gpu-0:9091" {
		t.Errorf("URL = %q, want http://gpu-0:9091 (runtime URL scheme change must be honored)", url)
	}
	if fp != validFP1 {
		t.Errorf("TLSFingerprint = %q, want unchanged %q - the Agent's pin must survive a runtime-URL-only scheme change", fp, validFP1)
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
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
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
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
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

// TestHandlePatchNode_JSONNullIsNoOpNotClear verifies spec section 16's
// amendment: a PATCH body containing tls_fingerprint: null (JSON null, not
// an explicit empty string) must NOT clear an existing pin. Go's
// encoding/json decodes both "field omitted" and "field explicitly null"
// to the same nil *string, so a NodePatch.TLSFingerprint of nil is - and
// must be treated as - "no change," exactly like every other NodePatch
// pointer field (MaxInFlight, GPUIndices). Only a non-nil "" clears
// (TestHandlePatchNode_ClearsExistingFingerprint above).
func TestHandlePatchNode_JSONNullIsNoOpNotClear(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200", rec.Code)
	}

	rec := patchNodeRequest(t, s, "gpu-0", `{"tls_fingerprint":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	fp := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if fp != validFP1 {
		t.Errorf("TLSFingerprint after tls_fingerprint:null = %q, want unchanged %q (null is a no-op, not a clear - see spec section 16)", fp, validFP1)
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
	r.SetNodeAgent("shared-host", true, 9200, "", "https")
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
	r.SetNodeAgent("shared-host", true, 9200, "", "https")
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
	r.SetNodeAgent("127.0.0.1", true, mustPortForTest(t, srv.URL), "", "https")
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
	r.SetNodeAgent("127.0.0.1", true, mustPortForTest(t, srv.URL), "super-secret-agent-token", "https")
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

// TestHandlePatchNode_RejectsURLOnlyMoveOntoConflictingSiblingHost verifies
// the fix for the gap the explicit-fingerprint sibling test above does not
// cover: a URL-only PATCH (no tls_fingerprint field at all) that moves an
// already-pinned node onto a host where a different pinned sibling already
// lives must be rejected before mutation, exactly like an explicit
// conflicting tls_fingerprint patch would be. Before the fix, validateTLSPatch
// only ran the sibling check when the patch itself set tls_fingerprint, so
// this URL-only move slipped through, silently carrying gpu-0's existing
// fingerprint onto shared-host and producing two NodeState entries sharing a
// Host with two different pinned fingerprints - the exact state section 15
// exists to prevent.
func TestHandlePatchNode_RejectsURLOnlyMoveOntoConflictingSiblingHost(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://solo-host:9091"},
		{Name: "gpu-1", URL: "https://shared-host:11435"},
	}, nil)
	r.SetNodeAgent("solo-host", true, 9200, "", "https")
	r.SetNodeAgent("shared-host", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-0: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec := patchNodeRequest(t, s, "gpu-1", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP2)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-1: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// URL-only move: no tls_fingerprint field in this request at all.
	rec := patchNodeRequest(t, s, "gpu-0", `{"url":"https://shared-host:11434"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("URL-only move onto conflicting sibling host: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	var gpu0 *router.NodeState
	for _, n := range nodes {
		if n.Name == "gpu-0" {
			gpu0 = n
		}
	}
	if gpu0 == nil {
		t.Fatal("gpu-0 not found after rejected patch")
	}
	gpu0.RLock()
	url, host, fp := gpu0.URL, gpu0.Host, gpu0.TLSFingerprint
	gpu0.RUnlock()
	if url != "https://solo-host:9091" || host != "solo-host" || fp != validFP1 {
		t.Errorf("gpu-0 state after rejected move: URL=%q Host=%q TLSFingerprint=%q, want URL=https://solo-host:9091 Host=solo-host TLSFingerprint=%q (unchanged)", url, host, fp, validFP1)
	}
}

// TestHandlePatchNode_AllowsURLOnlyMoveOntoIdenticalSiblingFingerprint
// verifies the fix does not over-reject: moving a pinned node by URL alone
// onto a host whose sibling already carries the SAME fingerprint is allowed,
// since it does not create a conflict.
func TestHandlePatchNode_AllowsURLOnlyMoveOntoIdenticalSiblingFingerprint(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://solo-host:9091"},
		{Name: "gpu-1", URL: "https://shared-host:11435"},
	}, nil)
	r.SetNodeAgent("solo-host", true, 9200, "", "https")
	r.SetNodeAgent("shared-host", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-0: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec := patchNodeRequest(t, s, "gpu-1", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-1 (same fingerprint): status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	rec := patchNodeRequest(t, s, "gpu-0", `{"url":"https://shared-host:11434"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("URL-only move onto identical-fingerprint sibling host: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	var gpu0 *router.NodeState
	for _, n := range nodes {
		if n.Name == "gpu-0" {
			gpu0 = n
		}
	}
	if gpu0 == nil {
		t.Fatal("gpu-0 not found after patch")
	}
	gpu0.RLock()
	url, host, fp := gpu0.URL, gpu0.Host, gpu0.TLSFingerprint
	gpu0.RUnlock()
	if url != "https://shared-host:11434" || host != "shared-host" || fp != validFP1 {
		t.Errorf("gpu-0 state after allowed move: URL=%q Host=%q TLSFingerprint=%q, want URL=https://shared-host:11434 Host=shared-host TLSFingerprint=%q", url, host, fp, validFP1)
	}
}

// TestHandlePatchNode_AllowsURLOnlyMoveOntoHostWithNoPinnedSibling verifies
// the fix does not over-reject: moving a pinned node by URL alone onto a
// host with no existing pinned sibling (or no sibling at all) is allowed.
func TestHandlePatchNode_AllowsURLOnlyMoveOntoHostWithNoPinnedSibling(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://solo-host:9091"},
		{Name: "gpu-1", URL: "https://empty-host:11435"},
	}, nil)
	r.SetNodeAgent("solo-host", true, 9200, "", "https")
	r.SetNodeAgent("empty-host", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("pin gpu-0: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	rec := patchNodeRequest(t, s, "gpu-0", `{"url":"https://empty-host:11434"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("URL-only move onto host with no pinned sibling: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	var gpu0 *router.NodeState
	for _, n := range nodes {
		if n.Name == "gpu-0" {
			gpu0 = n
		}
	}
	if gpu0 == nil {
		t.Fatal("gpu-0 not found after patch")
	}
	gpu0.RLock()
	url, fp := gpu0.URL, gpu0.TLSFingerprint
	gpu0.RUnlock()
	if url != "https://empty-host:11434" || fp != validFP1 {
		t.Errorf("gpu-0 state after allowed move: URL=%q TLSFingerprint=%q, want URL=https://empty-host:11434 TLSFingerprint=%q", url, fp, validFP1)
	}
}

// TestHandlePatchNode_ConcurrentPatchesCannotProduceConflictingSiblingPins is
// the regression test for the TOCTOU gap the review found in
// handlePatchNode's nodePatchMu fix: two concurrent PATCH requests to
// DIFFERENT node names, each individually valid against a point-in-time
// snapshot, must not be able to jointly land on host-A with two different
// non-empty pinned fingerprints.
//
// gpu-A starts pinned to FP1 on host-A; gpu-D is host-A's unpinned sibling
// (present so a stale-snapshot sibling check has something to miss). Two
// goroutines then race:
//   - move gpu-C onto host-A, explicitly pinning FP1 (matches gpu-A's
//     CURRENT pin, so it is valid if it runs first, or if it runs after
//     gpu-A is still at FP1)
//   - rotate gpu-A's own pin from FP1 to FP2
//
// Whichever of the two actually acquires nodePatchMu first fully completes
// (validate AND mutate) before the other's validateTLSPatch runs at all, so
// the second one's validation necessarily observes the first one's already-
// applied result. That guarantees exactly one of the two succeeds and the
// other is rejected with 409 - regardless of which one wins the race - and
// host-A can never end up with two disagreeing pinned fingerprints. Before
// the nodePatchMu fix, both goroutines could read a node-list snapshot
// before either mutated, both validate successfully against that shared
// stale snapshot, and both then apply - landing gpu-A at FP2 and gpu-C at
// FP1 on the same host simultaneously.
//
// A `ready` channel closed only after both goroutines are blocked on it is
// the ordering control (not a sleep): it guarantees both PATCH calls are
// in flight and genuinely contending for nodePatchMu, rather than the test
// merely running them one after another by construction.
func TestHandlePatchNode_ConcurrentPatchesCannotProduceConflictingSiblingPins(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-A", URL: "https://host-A:9091"},
		{Name: "gpu-D", URL: "https://host-A:9092"},
		{Name: "gpu-C", URL: "https://host-C:9093"},
	}, nil)
	r.SetNodeAgent("host-A", true, 9200, "", "https")
	r.SetNodeAgent("host-C", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-A", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed gpu-A pin: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	ready := make(chan struct{})
	var wg sync.WaitGroup
	var recA, recC *httptest.ResponseRecorder

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-ready
		recC = patchNodeRequest(t, s, "gpu-C", fmt.Sprintf(`{"url":"https://host-A:9093","tls_fingerprint":%q}`, validFP1))
	}()
	go func() {
		defer wg.Done()
		<-ready
		recA = patchNodeRequest(t, s, "gpu-A", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP2))
	}()
	close(ready)
	wg.Wait()

	successes, conflicts := 0, 0
	for _, rec := range []*httptest.ResponseRecorder{recA, recC} {
		switch rec.Code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d, body=%s", rec.Code, rec.Body.String())
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("got %d success(es) and %d conflict(s), want exactly 1 of each (recA.Code=%d body=%s; recC.Code=%d body=%s)",
			successes, conflicts, recA.Code, recA.Body.String(), recC.Code, recC.Body.String())
	}

	// The core invariant: no two nodes sharing a Host may end up with two
	// different non-empty pinned fingerprints, regardless of which goroutine
	// won the race above.
	nodes := r.Nodes()
	pinsByHost := make(map[string]string)
	for _, n := range nodes {
		n.RLock()
		host, fp := n.Host, n.TLSFingerprint
		n.RUnlock()
		if fp == "" {
			continue
		}
		if existing, ok := pinsByHost[host]; ok && existing != fp {
			t.Fatalf("host %q has conflicting pinned fingerprints %q and %q after concurrent PATCHes", host, existing, fp)
		}
		pinsByHost[host] = fp
	}
}

// TestHandlePatchNode_PinGatingUsesResultingHostAgentScheme is the regression
// test for a bug caught and fixed during this same change: the Agent-scheme
// gate on pinning must be evaluated against the PATCH's RESULTING host, never
// the node's CURRENT (pre-patch) host - a URL-only move can land a node on a
// completely different host with a different (or no) Agent configured, and a
// name-based lookup only ever resolves to the node's stale, current host.
// gpu-0 starts on old-host, which has NO Agent configured at all; a single
// request both moves it to new-host (Agent configured for https) AND pins a
// fingerprint. This must succeed - if the check incorrectly used old-host
// (has no Agent), it would wrongly reject; if it correctly uses new-host
// (Agent scheme https), it succeeds.
func TestHandlePatchNode_PinGatingUsesResultingHostAgentScheme(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://old-host:9091"},
	}, nil)
	r.SetNodeAgent("new-host", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"url":"https://new-host:9091","tls_fingerprint":%q}`, validFP1))
	if rec.Code != http.StatusOK {
		t.Fatalf("move+pin onto a host with a correctly-configured https Agent: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	url, host, fp := nodes[0].URL, nodes[0].Host, nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if url != "https://new-host:9091" || host != "new-host" || fp != validFP1 {
		t.Errorf("gpu-0 state after move+pin: URL=%q Host=%q TLSFingerprint=%q, want URL=https://new-host:9091 Host=new-host TLSFingerprint=%q", url, host, fp, validFP1)
	}
}

// TestHandlePatchNode_PinGatingIgnoresCurrentHostAgentScheme is the inverse
// of the test above: gpu-0 starts on a host that DOES have an https Agent
// configured, but the patch moves it to a DIFFERENT host with NO Agent
// configured at all. The pin must be rejected - proving the gate is keyed
// off the resulting host, not (incorrectly) satisfied by the node's stale
// current host just because a name-based lookup would have found it there.
func TestHandlePatchNode_PinGatingIgnoresCurrentHostAgentScheme(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://configured-host:9091"},
	}, nil)
	r.SetNodeAgent("configured-host", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"url":"https://unconfigured-host:9091","tls_fingerprint":%q}`, validFP1))
	if rec.Code != http.StatusConflict {
		t.Fatalf("move+pin onto a host with no Agent configured: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	url, host, fp := nodes[0].URL, nodes[0].Host, nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if url != "https://configured-host:9091" || host != "configured-host" || fp != "" {
		t.Errorf("gpu-0 state after rejected move+pin: URL=%q Host=%q TLSFingerprint=%q, want unchanged URL=https://configured-host:9091 Host=configured-host TLSFingerprint=\"\"", url, host, fp)
	}
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

// TestHandleEnableNodeAgent_RejectsSchemeDowngradeWhilePinned verifies P24's
// no-downgrade rule (section 7) at its actual enforcement point now that a
// pinned fingerprint describes the Agent's own scheme, not the runtime's:
// reconfiguring the Agent from https back to http while a fingerprint is
// still pinned for that host must be rejected (409), not silently accepted
// and left stranding an orphaned pin the next poll would fail closed on.
func TestHandleEnableNodeAgent_RejectsSchemeDowngradeWhilePinned(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Host: "gpu-0"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/agent", strings.NewReader(`{"port":9200,"scheme":"http"}`))
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleEnableNodeAgent(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("agent scheme downgrade while pinned: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	fp := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if fp != validFP1 {
		t.Errorf("TLSFingerprint = %q, want %q unchanged after rejected downgrade", fp, validFP1)
	}
}

// TestHandleEnableNodeAgent_ReconfigureOmittingSchemeKeepsExisting verifies
// the adversarial-review fix: a reconfigure call (e.g. rotating the port)
// that omits "scheme" entirely must NOT silently reset an existing https
// Agent back to http - it must keep the persisted scheme unless the caller
// explicitly says otherwise.
func TestHandleEnableNodeAgent_ReconfigureOmittingSchemeKeepsExisting(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Host: "gpu-0"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	// Enable with https explicitly.
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/agent", strings.NewReader(`{"port":9200,"scheme":"https"}`))
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleEnableNodeAgent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial enable: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// Reconfigure (e.g. rotate port) WITHOUT a "scheme" field at all.
	req2 := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/agent", strings.NewReader(`{"port":9201}`))
	req2.SetPathValue("name", "gpu-0")
	rec2 := httptest.NewRecorder()
	s.handleEnableNodeAgent(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reconfigure: status = %d, want 200, body=%s", rec2.Code, rec2.Body.String())
	}

	var resp struct {
		Scheme string `json:"scheme"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Scheme != "https" {
		t.Errorf("scheme after omitted-scheme reconfigure = %q, want %q (must not silently downgrade)", resp.Scheme, "https")
	}

	cfg, ok := r.NodeAgentSetting("gpu-0")
	if !ok || cfg.Scheme != "https" {
		t.Errorf("router NodeAgentConfig.Scheme = %q (ok=%v), want %q", cfg.Scheme, ok, "https")
	}
}

// TestHandleDisableNodeAgent_ClearsPinnedFingerprint verifies the
// adversarial-review fix: disabling the Node Agent entirely must clear any
// pinned TLS fingerprint on that host's nodes, not leave a stale/inert pin
// that shows as "protected" while nothing ever verifies it again.
func TestHandleDisableNodeAgent_ClearsPinnedFingerprint(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Host: "gpu-0"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	if rec := patchNodeRequest(t, s, "gpu-0", fmt.Sprintf(`{"tls_fingerprint":%q}`, validFP1)); rec.Code != http.StatusOK {
		t.Fatalf("seed pin: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/nodes/gpu-0/agent", nil)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleDisableNodeAgent(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disable: status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	fp := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if fp != "" {
		t.Errorf("TLSFingerprint after Agent disable = %q, want cleared (\"\")", fp)
	}
}

// TestHandleEnableNodeAgent_AllowsSchemeDowngradeWhenNoPin verifies the
// downgrade guard only fires when a fingerprint is actually pinned - a node
// with an Agent enabled but never pinned (or already cleared) can freely
// move between http and https.
func TestHandleEnableNodeAgent_AllowsSchemeDowngradeWhenNoPin(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Host: "gpu-0"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, 9200, "", "https")
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/agent", strings.NewReader(`{"port":9200,"scheme":"http"}`))
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handleEnableNodeAgent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent scheme downgrade with no pin: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}
