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

// Secrets persisted by sqliteStore (cloud provider API keys, marbor-issued
// runtime API keys, LiteLLM/HuggingFace/webhook tokens) are encrypted at
// rest with AES-256-GCM. The 32-byte master key comes from MARBOR_ENCRYPTION_KEY
// (base64-encoded) if set, otherwise from a "<db path>.key" file generated on
// first boot next to the database (0600, best-effort on platforms that
// support it). Values are prefixed "enc:v2:" so legacy plaintext rows (pre-
// upgrade) and already-encrypted rows are distinguishable; decryptSecret
// treats an unprefixed value as legacy plaintext rather than failing, so
// existing installs never break mid-upgrade - migrateEncryptSecrets re-
// encrypts them in place on the next boot.
const secretEncPrefix = "enc:v2:"

// secretEncPrefixV1 marks a pre-P137 blob: AES-256-GCM sealed with nil AAD,
// so any enc:v1: value decrypts successfully wherever placed under the same
// master key - a copy-paste between two secret-bearing rows/columns
// (compromised SQL access, buggy migration, restored backup) would succeed
// silently with the wrong plaintext. decryptSecret still reads this format
// for backward compatibility; encryptSecret never writes it again.
// migrateEncryptSecrets upgrades every v1 row to v2 (row-scoped AAD) on the
// next boot, same as it upgrades legacy unprefixed plaintext.
const secretEncPrefixV1 = "enc:v1:"

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
	// Write to a temp file then rename into place (P138): os.WriteFile writes
	// directly to keyPath, so a crash mid-write leaves a truncated/corrupt
	// file - the read path above hard-errors on anything that isn't exactly
	// 32 bytes, with no recovery short of deleting the file, which silently
	// drops every stored secret on next boot. Rename is atomic on POSIX and
	// on Windows for a same-volume rename (both paths are siblings here).
	tmpPath := keyPath + ".tmp"
	if err := os.WriteFile(tmpPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("store: write key file %s: %w", tmpPath, err)
	}
	_ = os.Chmod(tmpPath, 0o600) // best-effort; no-op on platforms without POSIX perms
	if err := os.Rename(tmpPath, keyPath); err != nil {
		return nil, fmt.Errorf("store: rename key file %s to %s: %w", tmpPath, keyPath, err)
	}
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
// unchanged throughout the codebase). aad binds the ciphertext to its
// intended location (P137, e.g. "cloud_providers.api_key" or
// "settings.litellm_api_key") - decryptSecret must be called with the exact
// same aad string, or GCM authentication fails rather than decrypting a
// value copy-pasted from a different row/column.
func encryptSecret(key []byte, aad, plaintext string) (string, error) {
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
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return secretEncPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptSecret reverses encryptSecret. A value without an enc: prefix is
// treated as legacy (pre-encryption) plaintext and returned unchanged - this
// is what lets an in-place upgrade keep working before migrateEncryptSecrets
// has had a chance to re-encrypt it, and is a deliberate never-fail fallback
// so a missing/rotated key degrades a single secret field rather than
// breaking store reads. aad must match the value passed to encryptSecret for
// this same field (P137); an enc:v1: value (pre-P137, no AAD binding) is
// still decrypted for backward compatibility - callers upgrade it to the
// current AAD-bound format via migrateEncryptSecrets, not on read.
func decryptSecret(key []byte, aad, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	var raw []byte
	var err error
	var sealAAD []byte
	switch {
	case strings.HasPrefix(value, secretEncPrefix):
		raw, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(value, secretEncPrefix))
		sealAAD = []byte(aad)
	case strings.HasPrefix(value, secretEncPrefixV1):
		raw, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(value, secretEncPrefixV1))
		sealAAD = nil
	default:
		return value, nil
	}
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
	plaintext, err := gcm.Open(nil, nonce, ciphertext, sealAAD)
	if err != nil {
		return "", fmt.Errorf("store: decryptSecret: wrong key or corrupt data: %w", err)
	}
	return string(plaintext), nil
}

// reencryptIfNeeded upgrades value to the current AAD-bound format
// (secretEncPrefix) if it is legacy plaintext or a legacy enc:v1: blob
// (nil-AAD), returning the possibly-updated value and whether a change is
// needed. A value already in the current format is returned unchanged with
// changed=false. Used by migrateEncryptSecrets so both plaintext-from-before-
// encryption-existed and v1-from-before-AAD-binding rows converge on the
// same current format on the next boot.
func reencryptIfNeeded(key []byte, aad, value string) (result string, changed bool, err error) {
	if value == "" || strings.HasPrefix(value, secretEncPrefix) {
		return value, false, nil
	}
	plain := value
	if strings.HasPrefix(value, secretEncPrefixV1) {
		plain, err = decryptSecret(key, aad, value)
		if err != nil {
			return "", false, err
		}
	}
	enc, err := encryptSecret(key, aad, plain)
	if err != nil {
		return "", false, err
	}
	return enc, true, nil
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
