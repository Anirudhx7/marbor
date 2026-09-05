package service

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEnsureAgentCert_GeneratesParsableMatchingPair verifies a fresh
// EnsureAgentCert call produces a certificate that parses, and whose public
// key actually matches the generated private key - the core TLS design doc
// requirement ("certificate is parseable and matches its own key").
func TestEnsureAgentCert_GeneratesParsableMatchingPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")

	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("EnsureAgentCert: %v", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatalf("cert file is not a valid PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		t.Fatalf("key file is not a valid PEM EC PRIVATE KEY block")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse EC private key: %v", err)
	}

	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("certificate public key is %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if !certPub.Equal(&key.PublicKey) {
		t.Fatal("certificate's public key does not match the generated private key")
	}
	if certPub.Curve.Params().Name != "P-256" {
		t.Errorf("curve = %s, want P-256", certPub.Curve.Params().Name)
	}

	validity := cert.NotAfter.Sub(cert.NotBefore)
	const tenYears = 10 * 365 * 24 * 60 * 60 // seconds, approximate
	if validity.Seconds() < tenYears-3600 {  // tolerate the 5-minute NotBefore backdate
		t.Errorf("validity = %s, want ~10 years", validity)
	}
}

// TestEnsureAgentCert_IdempotentOnValidExistingPair verifies re-running
// EnsureAgentCert (force=false) against an already-valid cert/key pair is a
// no-op - the exact requirement that keeps "agent service install" re-runs
// (upgrades, reinstalls) from silently invalidating a fingerprint the marbor
// already has pinned (spec section 3/7).
func TestEnsureAgentCert_IdempotentOnValidExistingPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")

	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("first EnsureAgentCert: %v", err)
	}
	firstCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	firstKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}

	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("second EnsureAgentCert: %v", err)
	}
	secondCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert after second call: %v", err)
	}
	secondKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key after second call: %v", err)
	}

	if string(firstCert) != string(secondCert) {
		t.Error("certificate changed across idempotent EnsureAgentCert calls, want unchanged")
	}
	if string(firstKey) != string(secondKey) {
		t.Error("key changed across idempotent EnsureAgentCert calls, want unchanged")
	}
}

// TestEnsureAgentCert_ForceRegeneratesExistingPair verifies force=true (the
// regen-cert subcommand's path, spec section 4) always produces a new
// key/cert even when a valid pair already exists.
func TestEnsureAgentCert_ForceRegeneratesExistingPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")

	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("first EnsureAgentCert: %v", err)
	}
	firstFingerprint, err := AgentCertFingerprint(certPath)
	if err != nil {
		t.Fatalf("AgentCertFingerprint: %v", err)
	}

	if err := EnsureAgentCert(certPath, keyPath, true); err != nil {
		t.Fatalf("forced EnsureAgentCert: %v", err)
	}
	secondFingerprint, err := AgentCertFingerprint(certPath)
	if err != nil {
		t.Fatalf("AgentCertFingerprint after regen: %v", err)
	}

	if firstFingerprint == secondFingerprint {
		t.Error("fingerprint unchanged after forced regeneration, want a new certificate")
	}
}

// TestEnsureAgentCert_RegeneratesOnCorruptExistingFiles verifies a
// missing/corrupt key (e.g. a truncated write from a prior crashed install)
// is treated as "not valid" and triggers regeneration rather than leaving
// the agent permanently unable to start.
func TestEnsureAgentCert_RegeneratesOnCorruptExistingFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")

	if err := os.WriteFile(certPath, []byte("not a real cert"), 0644); err != nil {
		t.Fatalf("seed corrupt cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a real key"), 0600); err != nil {
		t.Fatalf("seed corrupt key: %v", err)
	}

	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("EnsureAgentCert over corrupt files: %v", err)
	}
	if _, err := AgentCertFingerprint(certPath); err != nil {
		t.Fatalf("resulting certificate is still not valid: %v", err)
	}
}

// TestEnsureAgentCert_KeyFilePermissions verifies the private key file is
// written 0600 on POSIX (spec section 13's explicit requirement) - skipped
// on Windows, where os.Chmod cannot express POSIX mode bits and protection
// instead comes from the directory ACL (service_windows.go).
func TestEnsureAgentCert_KeyFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits don't apply on Windows - protection comes from the directory ACL instead, see service_windows.go")
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")

	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("EnsureAgentCert: %v", err)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0600 {
		t.Errorf("key file perms = %o, want 0600", perm)
	}
}

// TestAgentCertFingerprint_FormatMatchesRouterConvention verifies the
// fingerprint helper's output shape - "SHA256:" + 64 lowercase hex chars,
// no byte-separator colons - matches router.CertFingerprintSHA256's exact
// format (the two must never diverge, see AgentCertFingerprint's doc
// comment).
func TestAgentCertFingerprint_FormatMatchesRouterConvention(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")
	if err := EnsureAgentCert(certPath, keyPath, false); err != nil {
		t.Fatalf("EnsureAgentCert: %v", err)
	}

	fp, err := AgentCertFingerprint(certPath)
	if err != nil {
		t.Fatalf("AgentCertFingerprint: %v", err)
	}
	const prefix = "SHA256:"
	if len(fp) != len(prefix)+64 || fp[:len(prefix)] != prefix {
		t.Fatalf("fingerprint = %q, want SHA256: + 64 hex chars", fp)
	}
	for _, c := range fp[len(prefix):] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("fingerprint = %q, contains non-lowercase-hex character %q", fp, c)
		}
	}
}
