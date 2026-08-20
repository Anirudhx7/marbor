package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

// Secrets persisted by sqliteStore (cloud provider API keys, mesh-issued
// runtime API keys, LiteLLM/HuggingFace/webhook tokens) are encrypted at
// rest with AES-256-GCM. The 32-byte master key comes from MARBOR_ENCRYPTION_KEY
// (base64-encoded) if set, otherwise from a "<db path>.key" file generated on
// first boot next to the database (0600, best-effort on platforms that
// support it). Values are prefixed "enc:v1:" so legacy plaintext rows (pre-
// upgrade) and already-encrypted rows are distinguishable; decryptSecret
// treats an unprefixed value as legacy plaintext rather than failing, so
// existing installs never break mid-upgrade - migrateEncryptSecrets re-
// encrypts them in place on the next boot.
const secretEncPrefix = "enc:v1:"

const secretKeyEnvVar = "MARBOR_ENCRYPTION_KEY"

// loadOrCreateSecretKey resolves the 32-byte AES-256 master key. dbPath ""
// or ":memory:" (test/ephemeral stores) gets a random in-process key that is
// never persisted - fine, since nothing else about the store survives either.
func loadOrCreateSecretKey(dbPath string) ([]byte, error) {
	if envVal := os.Getenv(secretKeyEnvVar); envVal != "" {
		key, err := base64.StdEncoding.DecodeString(envVal)
		if err != nil {
			return nil, fmt.Errorf("store: %s is not valid base64: %w", secretKeyEnvVar, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("store: %s must decode to 32 bytes, got %d", secretKeyEnvVar, len(key))
		}
		return key, nil
	}

	if dbPath == "" || dbPath == ":memory:" {
		return randomKey()
	}

	keyPath := dbPath + ".key"
	if existing, err := os.ReadFile(keyPath); err == nil {
		if len(existing) == 32 {
			return existing, nil
		}
		return nil, fmt.Errorf("store: key file %s is %d bytes, want 32 (corrupt or foreign file)", keyPath, len(existing))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("store: read key file %s: %w", keyPath, err)
	}

	key, err := randomKey()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("store: write key file %s: %w", keyPath, err)
	}
	_ = os.Chmod(keyPath, 0o600) // best-effort; no-op on platforms without POSIX perms
	return key, nil
}

func randomKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("store: generate encryption key: %w", err)
	}
	return key, nil
}

// encryptSecret encrypts plaintext with AES-256-GCM under key, returning
// "" for "" (nothing to protect, and keeps empty-means-unset checks working
// unchanged throughout the codebase).
func encryptSecret(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("store: encryptSecret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("store: encryptSecret: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("store: encryptSecret nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return secretEncPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptSecret reverses encryptSecret. A value without the enc:v1: prefix
// is treated as legacy (pre-encryption) plaintext and returned unchanged -
// this is what lets an in-place upgrade keep working before
// migrateEncryptSecrets has had a chance to re-encrypt it, and is a
// deliberate never-fail fallback so a missing/rotated key degrades a single
// secret field rather than breaking store reads.
func decryptSecret(key []byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, secretEncPrefix) {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, secretEncPrefix))
	if err != nil {
		return "", fmt.Errorf("store: decryptSecret: bad encoding: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("store: decryptSecret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("store: decryptSecret: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("store: decryptSecret: ciphertext too short")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("store: decryptSecret: wrong key or corrupt data: %w", err)
	}
	return string(plaintext), nil
}

// sensitiveSettingKeys are the settings-table keys holding secrets rather
// than plain scalars. GetSetting/SetSetting encrypt/decrypt transparently
// only for these - every other setting (ports, toggles, strategy names)
// passes through untouched so callers comparing "true"/"false" or parsing
// integers are unaffected.
var sensitiveSettingKeys = map[string]bool{
	"litellm_api_key":   true,
	"huggingface_token": true,
	"webhook_secret":    true,
}
