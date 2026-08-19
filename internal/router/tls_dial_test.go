package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

// psHandler is the same minimal /api/ps handler body as nodePSServer
// (agent_poll_test.go), factored out so an httptest.NewTLSServer here can
// use it directly without spinning up (and leaking) a second, unused plain
// HTTP server just to borrow its Handler.
func psHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"models": []map[string]interface{}{}})
	})
}

// TestPatchNodeTLSFingerprint verifies PatchNode's merge semantics for the
// new TLSFingerprint field: applies a pin, a merge update touching only an
// unrelated field does not clobber it, and a non-nil empty string explicitly
// clears it back to "" (mirrors TestPatchNodeGPUIndices' convention).
func TestPatchNodeTLSFingerprint(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://gpu-0:9091"},
	}, nil)

	fp := "SHA256:aa:bb:cc:dd"
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &fp}) {
		t.Fatal("PatchNode returned false for existing node")
	}
	r.nodes[0].mu.RLock()
	got := r.nodes[0].TLSFingerprint
	r.nodes[0].mu.RUnlock()
	if got != fp {
		t.Fatalf("TLSFingerprint = %q, want %q", got, fp)
	}

	model := "NVIDIA RTX 4090"
	if !r.PatchNode("gpu-0", NodePatch{GPUModel: &model}) {
		t.Fatal("PatchNode (unrelated merge) returned false")
	}
	r.nodes[0].mu.RLock()
	got = r.nodes[0].TLSFingerprint
	r.nodes[0].mu.RUnlock()
	if got != fp {
		t.Fatalf("TLSFingerprint after unrelated merge = %q, want preserved %q", got, fp)
	}

	empty := ""
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &empty}) {
		t.Fatal("PatchNode (clear) returned false")
	}
	r.nodes[0].mu.RLock()
	got = r.nodes[0].TLSFingerprint
	r.nodes[0].mu.RUnlock()
	if got != "" {
		t.Fatalf("TLSFingerprint after clear = %q, want empty", got)
	}
}

// TestUpdateNodeURL_CarriesTLSFingerprint verifies a node's pinned
// fingerprint survives a URL edit (host/port change) that keeps the scheme
// https:// - UpdateNodeURL reconstructs a fresh NodeState rather than
// mutating in place (see its doc comment), so any field not explicitly
// carried over would otherwise silently reset to the zero value.
func TestUpdateNodeURL_CarriesTLSFingerprint(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://old-host:9091"},
	}, nil)

	sum := sha256.Sum256([]byte("test"))
	fp := "SHA256:" + hex.EncodeToString(sum[:])
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &fp}) {
		t.Fatal("PatchNode returned false")
	}

	if err := r.UpdateNodeURL("gpu-0", "https://new-host:9092"); err != nil {
		t.Fatalf("UpdateNodeURL: %v", err)
	}

	nodes := r.Nodes()
	nodes[0].RLock()
	got := nodes[0].TLSFingerprint
	nodes[0].RUnlock()
	if got != fp {
		t.Errorf("TLSFingerprint after UpdateNodeURL = %q, want preserved %q", got, fp)
	}
}

// certFingerprint computes the "SHA256:..." fingerprint of an
// httptest.Server's leaf certificate, the same format dialTLSContext
// computes at handshake time.
func certFingerprint(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	if len(srv.Certificate().Raw) == 0 {
		t.Fatal("test server certificate has no Raw bytes")
	}
	sum := sha256.Sum256(srv.Certificate().Raw)
	return "SHA256:" + hex.EncodeToString(sum[:])
}

// TestHTTPClientForNode_PinnedFingerprintMatch proves the happy path: a node
// registered with the correct pinned fingerprint for its Node Agent's host:
// port can be reached over HTTPS through HTTPClientForNode.
func TestHTTPClientForNode_PinnedFingerprintMatch(t *testing.T) {
	srv := httptest.NewTLSServer(psHandler())
	defer srv.Close()
	port := mustPort(t, srv.URL)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", Host: "127.0.0.1"},
	}, nil)
	r.SetNodeAgent("127.0.0.1", true, port, "", "http")

	fp := certFingerprint(t, srv)
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &fp}) {
		t.Fatal("PatchNode returned false")
	}

	client := r.HTTPClientForNode(5 * time.Second)
	resp, err := client.Get(srv.URL + "/api/ps")
	if err != nil {
		t.Fatalf("Get with correct pinned fingerprint failed: %v", err)
	}
	defer resp.Body.Close()
}

// TestHTTPClientForNode_PinnedFingerprintMismatch proves the fail-closed
// path (spec section 6/7): a node pinned to the WRONG fingerprint for its
// Node Agent's host:port must have every request over that transport
// refused at the TLS handshake, never silently trusted or downgraded to
// plaintext.
func TestHTTPClientForNode_PinnedFingerprintMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(psHandler())
	defer srv.Close()
	port := mustPort(t, srv.URL)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", Host: "127.0.0.1"},
	}, nil)
	r.SetNodeAgent("127.0.0.1", true, port, "", "http")

	wrongFP := "SHA256:" + hex.EncodeToString(sha256.New().Sum(nil))
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &wrongFP}) {
		t.Fatal("PatchNode returned false")
	}

	client := r.HTTPClientForNode(5 * time.Second)
	_, err := client.Get(srv.URL + "/api/ps")
	if err == nil {
		t.Fatal("Get with mismatched pinned fingerprint succeeded, want fail-closed refusal")
	}
	// agent_poll.go's mismatch-vs-generic-unreachable distinction (P24
	// section 6, AgentTLSMismatch) depends entirely on errors.Is reaching
	// ErrTLSFingerprintMismatch through however many layers Go's crypto/tls
	// (which wraps a VerifyPeerCertificate error in its own
	// *tls.CertificateVerificationError) and this package's own %w wrapping
	// add on top - assert that chain actually holds, not just that *some*
	// error was returned.
	if !errors.Is(err, ErrTLSFingerprintMismatch) {
		t.Errorf("errors.Is(err, ErrTLSFingerprintMismatch) = false, want true (err: %v)", err)
	}
}

// TestHTTPClientForNode_AmbiguousSiblingFingerprints proves section 15's
// dial-time safeguard: two NodeState entries sharing one Host with two
// different non-empty pinned fingerprints must refuse the dial outright
// rather than silently picking one - task #4's admin-layer check is meant
// to prevent this state from arising, but the dial-time code must never
// guess if it does.
func TestHTTPClientForNode_AmbiguousSiblingFingerprints(t *testing.T) {
	srv := httptest.NewTLSServer(psHandler())
	defer srv.Close()
	port := mustPort(t, srv.URL)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", Host: "127.0.0.1"},
		{Name: "gpu-1", URL: "http://127.0.0.1:2", Host: "127.0.0.1"},
	}, nil)
	r.SetNodeAgent("127.0.0.1", true, port, "", "http")

	fp := certFingerprint(t, srv)
	otherFP := "SHA256:" + hex.EncodeToString(sha256.New().Sum(nil))
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &fp}) {
		t.Fatal("PatchNode gpu-0 returned false")
	}
	if !r.PatchNode("gpu-1", NodePatch{TLSFingerprint: &otherFP}) {
		t.Fatal("PatchNode gpu-1 returned false")
	}

	client := r.HTTPClientForNode(5 * time.Second)
	_, err := client.Get(srv.URL + "/api/ps")
	if err == nil {
		t.Fatal("Get with ambiguous sibling fingerprints succeeded, want refusal")
	}
}

// TestHTTPClientForNode_UnpinnedNodeAgentFailsClosed proves the "no pin
// recorded but URL is https://" case (spec section 6): with no fingerprint
// pinned at all, a self-signed cert fails ordinary certificate verification
// and the dial errors out as unreachable - the correct fail-safe default,
// requiring no special-case code.
func TestHTTPClientForNode_UnpinnedNodeAgentFailsClosed(t *testing.T) {
	srv := httptest.NewTLSServer(psHandler())
	defer srv.Close()
	port := mustPort(t, srv.URL)

	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", Host: "127.0.0.1"},
	}, nil)
	r.SetNodeAgent("127.0.0.1", true, port, "", "http")

	client := r.HTTPClientForNode(5 * time.Second)
	_, err := client.Get(srv.URL + "/api/ps")
	if err == nil {
		t.Fatal("Get against an unpinned self-signed node agent succeeded, want standard-verification failure")
	}
}
