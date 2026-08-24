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
	"math/big"
	"os"
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
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("service: create agent TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("service: marshal agent TLS key: %w", err)
	}

	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0644); err != nil {
		return fmt.Errorf("service: write agent TLS certificate %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return fmt.Errorf("service: write agent TLS key %s: %w", keyPath, err)
	}
	return nil
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
