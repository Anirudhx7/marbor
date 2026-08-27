// Package service (see service.go's package doc) also owns Marbor Agent TLS
// certificate generation (P24). This file is platform-agnostic - stdlib
// crypto/tls/crypto/x509/crypto/ecdsa only, Law 4 (zero external
// dependency) - deliberately placed in package service rather than package
// marboragent (as .local/core/P24-TLS-DESIGN.md section 9 originally
// suggested): each platform's Install (service_linux.go/service_darwin.go/
// service_windows.go) must call this at install time, and package marboragent
// already imports package service, so package service importing back from
// marboragent would be a compile-time cycle. This file never imports
// marboragent - only stdlib.
package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certValidity is 10 years: this is TOFU pinning (see
// .local/specs/node-agent-tls.md section 1), not PKI, so there is no
// meaningfully-short expiry to force rotation against - a long validity
// avoids background rotation-before-expiry complexity this design
// deliberately does not need. Rotation, when wanted, is the operator-driven
// regen-cert path, never automatic.
const certValidity = 10 * 365 * 24 * time.Hour

// EnsureAgentCert generates a self-signed ECDSA P-256 certificate/key pair
// at certPath/keyPath if they don't already exist as a valid, matching
// pair, and is a no-op (returns nil immediately) otherwise - so re-running
// "agent service install" (upgrades, reinstalls) never silently regenerates
// a cert and invalidates a fingerprint marbor already has pinned (spec
// section 3's idempotency requirement). Pass force=true (the regen-cert
// subcommand's forced-regeneration path, spec section 4) to always
// (re)generate regardless of what's currently on disk.
//
// The certificate file is written 0644 (it is not secret - only the private
// key is); the key file is written 0600. Callers on Windows must also apply
// an ACL restriction to the containing directory (os.Chmod cannot express
// POSIX mode bits there) - see service_windows.go's restrictToSystemAdmins.
func EnsureAgentCert(certPath, keyPath string, force bool) error {
	if !force {
		if certValid, keyValid := certAndKeyValid(certPath, keyPath); certValid && keyValid {
			return nil
		}
		// Log loudly whenever a pre-existing cert file is about to be
		// replaced by an unforced call (expired, corrupt, or mismatched
		// pair) - regeneration changes the pinned fingerprint, and an
		// operator who doesn't know that happened will hit a confusing TLS
		// handshake failure against marbor until they re-confirm the new
		// fingerprint.
		if _, err := os.Stat(certPath); err == nil {
			log.Printf("service: existing TLS certificate/key at %s / %s is invalid or expired - regenerating (this changes the pinned fingerprint; re-confirm and re-pin this node from the marbor admin UI/CLI)", certPath, keyPath)
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("service: generate agent TLS key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("service: generate agent TLS certificate serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: Name},
		// NotBefore backdated 5 minutes: tolerates modest clock skew between
		// the node generating this cert and marbor verifying it later,
		// without weakening the pin itself (pinning is by fingerprint, not
		// by validity window).
		NotBefore:   time.Now().Add(-5 * time.Minute),
		NotAfter:    time.Now().Add(certValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        false,
		DNSNames:    dnsNamesForCert(),
		IPAddresses: ipAddressesForCert(),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("service: create agent TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("service: marshal agent TLS key: %w", err)
	}

	// Write both files atomically via a temp-then-rename in the same
	// directory (rename is atomic on the same filesystem, both POSIX and
	// Windows) so a crash between the two writes can never leave a
	// mismatched cert/key pair on disk - certAndKeyValid would treat a
	// mismatched pair as invalid and EnsureAgentCert would regenerate a
	// third keypair, permanently invalidating whatever fingerprint marbor
	// had pinned. Key is written first, then cert: if the process dies after
	// only the key lands, the next run sees no cert file and regenerates
	// cleanly; the reverse ordering would leave a cert with no matching key.
	if err := writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return fmt.Errorf("service: write agent TLS key %s: %w", keyPath, err)
	}
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0644); err != nil {
		return fmt.Errorf("service: write agent TLS certificate %s: %w", certPath, err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in path's directory, then
// renames it into place - avoiding the truncate-then-write window a plain
// os.WriteFile leaves open where a crash mid-write corrupts an existing
// file or leaves a partially-written new one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// dnsNamesForCert/ipAddressesForCert populate the certificate's Subject
// Alternative Name fields, which standard TLS clients require (Go's own
// crypto/tls rejects a cert with no SAN when verifying, even a self-signed
// one) - harmless under this app's own fingerprint-pinning verifier
// (net/http's default verification is never used here), but confusing when
// an operator manually probes the endpoint with curl/openssl to debug.
// Best-effort:
// hostname/interface-address lookup failures just mean an emptier (but
// still valid) SAN list, never a generation failure.
func dnsNamesForCert() []string {
	names := []string{"localhost"}
	if host, err := os.Hostname(); err == nil && host != "" {
		names = append(names, host)
	}
	return names
}

func ipAddressesForCert() []net.IP {
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ips = append(ips, ipNet.IP)
	}
	return ips
}

// certAndKeyValid reports whether certPath/keyPath both exist, parse as a
// well-formed PEM certificate/EC private key pair, and the key's public
// half actually matches the certificate's - the idempotency check
// EnsureAgentCert uses to decide no-op vs (re)generation. A corrupt or
// partial pair (e.g. a truncated write from a prior crashed install) is
// correctly treated as "not valid," triggering regeneration rather than
// leaving the agent unable to start.
func certAndKeyValid(certPath, keyPath string) (certOK, keyOK bool) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false, false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return false, false
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return false, false
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return false, false
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		// Expired (or not-yet-valid, which should never happen given the
		// 5-minute backdate above) - treat as invalid so EnsureAgentCert
		// regenerates instead of trusting a cert forever, per certValidity's
		// otherwise-permanent idempotency gate.
		return false, false
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return true, false
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return true, false
	}
	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certPub.Equal(&key.PublicKey) {
		return true, false
	}
	return true, true
}

// AgentCertFingerprint reads certPath and returns its SHA-256 fingerprint
// as "SHA256:<64 hex chars>" - deliberately the exact same format and
// computation (over the certificate's raw DER bytes) as
// router.CertFingerprintSHA256 on the marbor side, so a value an operator
// reads via "agent service status" always matches what the marbor's
// tls-probe endpoint reports for the same certificate. The two are
// necessarily separate implementations (package service cannot import
// package router, a marbor-side package, from the Marbor agent binary) - if
// this computation ever changes, router.CertFingerprintSHA256 must change
// identically, and vice versa.
func AgentCertFingerprint(certPath string) (string, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return "", fmt.Errorf("service: read agent TLS certificate %s: %w", certPath, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("service: %s is not a valid PEM certificate", certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	return "SHA256:" + hex.EncodeToString(sum[:]), nil
}
