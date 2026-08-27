package router

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CertFingerprintSHA256 computes the "SHA256:<64 lowercase hex chars>"
// fingerprint of a raw DER certificate - the exact format dialTLSContext's
// VerifyPeerCertificate verifies pinned nodes against below, and the format
// admin.go's tls-probe endpoint must report to the operator for
// confirmation (P24 section 2). The two call sites must never independently
// reimplement this computation - a silent formatting divergence (e.g. one
// side adding byte-separator colons) would mean a value the operator copied
// from a probe response could never actually match at verification time.
func CertFingerprintSHA256(rawCert []byte) string {
	sum := sha256.Sum256(rawCert)
	return "SHA256:" + hex.EncodeToString(sum[:])
}

// HTTPClientForNode returns an *http.Client sharing the Router's single
// TLS-pinning-aware Transport (see dialTLSContext below), with the given
// per-call timeout. Every HTTP client that talks to a Marbor Agent - the poll
// path (agent_poll.go), the model eviction path (eviction.go), and every
// admin action-path call site (internal/admin/admin.go: pull/list/delete/
// unload, runtime start/stop/restart/logs, health checks) - must be built
// via this helper instead of a bare &http.Client{} literal. That is what
// makes fingerprint verification apply uniformly rather than only to
// whichever call sites happened to be updated (P24, see
// .local/specs/node-agent-tls.md section 6, decision #3 in
// .local/core/P24-TLS-DESIGN.md: poll and action paths ship together, not
// staged).
func (r *Router) HTTPClientForNode(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if r.tlsTransport != nil {
		// Do not assign a nil *http.Transport into the Transport interface
		// field even when this Router was constructed directly (bypassing
		// New(), a pattern several existing router tests use) - a typed nil
		// pointer stored in an interface is not an interface nil, so
		// net/http's own "Transport == nil -> use DefaultTransport" check
		// would not catch it, and calling through a nil *http.Transport
		// panics inside net/http's RoundTrip machinery.
		client.Transport = r.tlsTransport
	}
	return client
}

// dialTLSContext is the Router's shared http.Transport.DialTLSContext,
// installed once in New() (via a closure, since the Transport is built
// before the Router itself exists - see New()'s rr variable) and reused by
// every client HTTPClientForNode returns, plus the poll-path client. See
// .local/specs/node-agent-tls.md section 6.
//
// Target-to-NodeState mapping (verified against the current codebase before
// this was written, since agent_poll.go groups multiple NodeState entries
// under one shared physical Host/agent - section 15's amendment):
//
//   - The dial address Go's http.Transport passes here is always the
//     request URL's host:port - net.SplitHostPort parses both IPv4 and the
//     bracketed IPv6 form correctly, and every marbor-agent request URL in
//     this codebase is built with an explicit port (buildAgentURL,
//     buildAgentUnloadURL, admin.go's per-action URLs), so there is never
//     an implicit default-port case to reconcile.
//   - r.marborAgents is keyed by bare Host with exactly one MarborAgentConfig
//     (one port) per host - so a dial address unambiguously either matches
//     "this is host H's Marbor Agent" or it doesn't. A non-matching address
//     (runtime-probe traffic sharing this same client/Transport, since
//     New() passes it to runtimepkg.NewProbe too, or any other https
//     target) falls through to ordinary certificate verification -
//     unaffected by pinning, exactly as if this Transport were unmodified.
//   - The lookup is live at every dial call (r.mu.RLock() + each NodeState's
//     own RLock()), never cached, so a concurrent PatchNode/UpdateNodeURL
//     is picked up by the very next dial with no staleness window; multiple
//     concurrent dials only ever take read locks, so they do not serialize
//     against each other.
//   - A URL change (UpdateNodeURL) replaces the *NodeState but this
//     function only ever reads Host/TLSFingerprint through r.nodes at dial
//     time, so it always sees whatever the current node list holds - no
//     dedicated invalidation needed.
//   - Redirects: Go's http.Client re-invokes DialTLSContext for whatever
//     new host:port a redirect targets, going through this exact same
//     lookup - if the new target isn't a known marbor-agent endpoint it
//     naturally falls through to ordinary verification. Marbor Agent
//     endpoints do not redirect in practice (R9's protocol is plain JSON
//     GET/POST), so this is a non-issue in practice but the logic handles
//     it safely regardless.
//   - Test-server addresses (httptest.NewTLSServer, 127.0.0.1:<random>)
//     match this same lookup as long as the test registers a NodeState with
//     Host="127.0.0.1" and a MarborAgentConfig{Port: <that port>} - no special
//     casing needed for tests.
//   - Section 15's multi-GPU-per-host case: today, nothing prevents two
//     NodeState entries sharing one Host from carrying different pinned
//     fingerprints for what is physically one certificate (task #4's
//     admin-layer sibling-consistency check is what will prevent this from
//     arising going forward - not yet implemented as of this file). This
//     function must never silently pick one of several disagreeing answers,
//     so pinnedFingerprintFor reports that case as ambiguous and the dial is
//     refused outright (fail closed) rather than guessed.
func (r *Router) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// A stock http.Transport normalizes a port-less https address to
		// host:443 before ever reaching a custom DialTLSContext; this one
		// didn't, so a node configured as "https://gpu.example.com" (no
		// port) failed every health check with a confusing split error
		// instead of the port-443 default a plain https:// URL implies.
		// Retry once against the normalized form before actually failing -
		// the pinned-lookup/dial logic below is unchanged either way, so
		// this doesn't weaken the fail-closed-on-ambiguous pinning check.
		host, portStr, err = net.SplitHostPort(addr + ":443")
		if err != nil {
			return nil, fmt.Errorf("router: dialTLSContext: split %q: %w", addr, err)
		}
		addr = net.JoinHostPort(host, portStr)
	}

	fingerprint, matched, ambiguous := r.pinnedFingerprintFor(host, portStr)
	if ambiguous {
		return nil, fmt.Errorf("router: dialTLSContext: host %q has multiple sibling nodes with conflicting pinned TLS fingerprints - refusing to guess which one is correct (see .local/specs/node-agent-tls.md section 15)", host)
	}

	dialer := &net.Dialer{}
	rawConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	if !matched || fingerprint == "" {
		// Not a known Marbor Agent endpoint, or a Marbor Agent host with no pin
		// declared yet: ordinary verified TLS, exactly what the zero-value
		// Transport would have done for this address. A still-unpinned
		// self-signed cert simply fails standard verification here and the
		// dial errors out as "unreachable" - the correct fail-safe default
		// for "no pin recorded but URL is https://" (section 6) without any
		// special-case code.
		tlsConn := tls.Client(rawConn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}

	tlsConn := tls.Client(rawConn, &tls.Config{
		// Pinning replaces PKI trust entirely for a pinned node - no CA
		// chain is checked (spec section 1); VerifyPeerCertificate below is
		// the only trust decision made.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no certificate presented")
			}
			got := CertFingerprintSHA256(rawCerts[0])
			if !strings.EqualFold(got, fingerprint) {
				return fmt.Errorf("tls fingerprint mismatch for %s: expected %s got %s - refusing connection, possible MITM or cert rotation: %w", host, fingerprint, got, ErrTLSFingerprintMismatch)
			}
			return nil
		},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls fingerprint mismatch or handshake failure for %s: %w", host, err)
	}
	return tlsConn, nil
}

// ErrTLSFingerprintMismatch is wrapped into the error VerifyPeerCertificate
// returns above when a pinned node presents a certificate that doesn't
// match its pinned fingerprint. Go's crypto/tls wraps a VerifyPeerCertificate
// error in a *tls.CertificateVerificationError (itself Unwrap-able) before
// returning it from Handshake, and dialTLSContext's own wrap above uses %w
// too - so errors.Is(err, ErrTLSFingerprintMismatch) correctly reaches
// through both layers from a caller holding only the final Do(req) error
// (agent_poll.go uses this to distinguish a mismatch from any other
// unreachable-node failure, surfaced to the dashboard as a distinct status
// per .local/specs/node-agent-tls.md section 6).
var ErrTLSFingerprintMismatch = errors.New("tls fingerprint mismatch")

// pinnedFingerprintFor reports the single pinned TLS fingerprint that
// applies to a dial address's host:port, if that address is a known Node
// Agent endpoint.
//
// matched is true iff host:port corresponds to some host's configured Node
// Agent (r.marborAgents, keyed by bare Host) - false means this dial address
// isn't a Marbor Agent endpoint at all (e.g. runtime-probe traffic sharing the
// same client/Transport) and pinning must not apply to it.
//
// ambiguous is true when two or more NodeState entries sharing that Host
// (section 15's multi-GPU-per-host case) carry different non-empty pinned
// fingerprints for what is physically one certificate - a state task #4's
// admin-layer sibling-consistency check is meant to prevent from arising,
// but this dial-time check must never silently pick one of several
// disagreeing answers.
func (r *Router) pinnedFingerprintFor(host, portStr string) (fingerprint string, matched bool, ambiguous bool) {
	r.mu.RLock()
	cfg, ok := r.marborAgents[host]
	if !ok || strconv.Itoa(cfg.Port) != portStr {
		r.mu.RUnlock()
		return "", false, false
	}
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, n := range nodes {
		n.RLock()
		sameHost := n.Host == host
		fp := n.TLSFingerprint
		n.RUnlock()
		if sameHost && fp != "" {
			seen[fp] = struct{}{}
		}
	}
	if len(seen) > 1 {
		return "", true, true
	}
	for fp := range seen {
		return fp, true, false
	}
	return "", true, false
}
