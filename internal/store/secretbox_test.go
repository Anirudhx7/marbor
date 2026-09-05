package store

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustRandomKey(t *testing.T) []byte {
	t.Helper()
	k, err := randomKey()
	if err != nil {
		t.Fatalf("randomKey: %v", err)
	}
	return k
}

func TestEncryptDecryptSecretRoundTrip(t *testing.T) {
	key := mustRandomKey(t)
	enc, err := encryptSecret(key, "test.field", "sk-super-secret")
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	if enc == "sk-super-secret" {
		t.Fatalf("encryptSecret did not transform the plaintext")
	}
	if !strings.HasPrefix(enc, secretEncPrefix) {
		t.Fatalf("encryptSecret output missing %q prefix: %q", secretEncPrefix, enc)
	}
	dec, err := decryptSecret(key, "test.field", enc)
	if err != nil {
		t.Fatalf("decryptSecret: %v", err)
	}
	if dec != "sk-super-secret" {
		t.Fatalf("decryptSecret = %q, want %q", dec, "sk-super-secret")
	}
}

func TestEncryptSecretEmptyStringPassthrough(t *testing.T) {
	key := mustRandomKey(t)
	enc, err := encryptSecret(key, "test.field", "")
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	if enc != "" {
		t.Fatalf("encryptSecret(\"\") = %q, want empty", enc)
	}
	dec, err := decryptSecret(key, "test.field", "")
	if err != nil {
		t.Fatalf("decryptSecret: %v", err)
	}
	if dec != "" {
		t.Fatalf("decryptSecret(\"\") = %q, want empty", dec)
	}
}

// TestDecryptSecretLegacyPlaintextPassthrough is the guard that keeps an
// in-place upgrade from breaking: any value without an enc: prefix must
// come back unchanged, not error out, so old rows work until
// migrateEncryptSecrets re-encrypts them.
func TestDecryptSecretLegacyPlaintextPassthrough(t *testing.T) {
	key := mustRandomKey(t)
	dec, err := decryptSecret(key, "test.field", "sk-plaintext-legacy-value")
	if err != nil {
		t.Fatalf("decryptSecret: %v", err)
	}
	if dec != "sk-plaintext-legacy-value" {
		t.Fatalf("decryptSecret = %q, want unchanged legacy plaintext", dec)
	}
}

func TestDecryptSecretWrongKeyFails(t *testing.T) {
	keyA := mustRandomKey(t)
	keyB := mustRandomKey(t)
	enc, err := encryptSecret(keyA, "test.field", "sk-secret")
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	if _, err := decryptSecret(keyB, "test.field", enc); err == nil {
		t.Fatalf("decryptSecret with wrong key: want error, got nil")
	}
}

// TestDecryptSecretWrongAADFails verifies the row-scoped AAD binding: a
// ciphertext sealed for one field must fail GCM authentication (not silently
// decrypt) when Open is called with a different field's AAD - the guard
// against a copy-pasted value across rows/columns.
func TestDecryptSecretWrongAADFails(t *testing.T) {
	key := mustRandomKey(t)
	enc, err := encryptSecret(key, "cloud_providers.api_key", "sk-secret")
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	if _, err := decryptSecret(key, "runtime_keys.key", enc); err == nil {
		t.Fatalf("decryptSecret with mismatched AAD: want error, got nil")
	}
}

func TestLoadOrCreateSecretKeyPersistsAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	k1, err := loadOrCreateSecretKey(dbPath)
	if err != nil {
		t.Fatalf("loadOrCreateSecretKey (first): %v", err)
	}
	keyFile := dbPath + ".key"
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("expected key file %s to exist: %v", keyFile, err)
	}
	k2, err := loadOrCreateSecretKey(dbPath)
	if err != nil {
		t.Fatalf("loadOrCreateSecretKey (second): %v", err)
	}
	if string(k1) != string(k2) {
		t.Fatalf("secret key changed across reopen - would break decryption of existing secrets")
	}
}

func TestLoadOrCreateSecretKeyEnvVarOverride(t *testing.T) {
	key := mustRandomKey(t)
	t.Setenv(secretKeyEnvVar, base64.StdEncoding.EncodeToString(key))
	dbPath := filepath.Join(t.TempDir(), "test.db")
	got, err := loadOrCreateSecretKey(dbPath)
	if err != nil {
		t.Fatalf("loadOrCreateSecretKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("loadOrCreateSecretKey ignored %s env var", secretKeyEnvVar)
	}
	if _, err := os.Stat(dbPath + ".key"); err == nil {
		t.Fatalf("key file should not be created when %s is set", secretKeyEnvVar)
	}
}

func TestLoadOrCreateSecretKeyEnvVarWrongLength(t *testing.T) {
	t.Setenv(secretKeyEnvVar, "dG9vc2hvcnQ=") // base64("tooshort"), not 32 bytes
	if _, err := loadOrCreateSecretKey(filepath.Join(t.TempDir(), "test.db")); err == nil {
		t.Fatalf("loadOrCreateSecretKey: want error for wrong-length key, got nil")
	}
}

func TestLoadOrCreateSecretKeyEphemeralForInMemory(t *testing.T) {
	k1, err := loadOrCreateSecretKey(":memory:")
	if err != nil {
		t.Fatalf("loadOrCreateSecretKey: %v", err)
	}
	k2, err := loadOrCreateSecretKey(":memory:")
	if err != nil {
		t.Fatalf("loadOrCreateSecretKey: %v", err)
	}
	if string(k1) == string(k2) {
		t.Fatalf("expected independent random keys for :memory: stores, got the same key")
	}
}

// TestMigrateEncryptSecretsUpgradesLegacyPlaintext simulates an existing
// install that has plaintext secrets in cloud_providers/runtime_keys/settings
// from before at-rest encryption existed. Reopening the store must encrypt
// them in place without any manual step, and every read path must keep
// returning the original plaintext to callers.
func TestMigrateEncryptSecretsUpgradesLegacyPlaintext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := st.(*sqliteStore)

	// Write plaintext directly to bypass the encrypting UpsertX/SetSetting
	// paths, reproducing pre-upgrade on-disk state.
	if _, err := ss.db.Exec(`INSERT INTO cloud_providers (name, provider, base_url, api_key) VALUES (?, ?, ?, ?)`,
		"anthropic", "anthropic", "https://api.anthropic.com", "sk-ant-legacy-plaintext"); err != nil {
		t.Fatalf("seed cloud_providers: %v", err)
	}
	if _, err := ss.db.Exec(`INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"team-a", "sk-marbor-legacy-plaintext", 0, 0, 0, "[]", 0); err != nil {
		t.Fatalf("seed runtime_keys: %v", err)
	}
	if _, err := ss.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`,
		"litellm_api_key", "sk-litellm-legacy-plaintext"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must run migrateEncryptSecrets and encrypt the legacy rows.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer st2.Close()
	ss2 := st2.(*sqliteStore)

	var rawAPIKey string
	if err := ss2.db.QueryRow(`SELECT api_key FROM cloud_providers WHERE name=?`, "anthropic").Scan(&rawAPIKey); err != nil {
		t.Fatalf("select raw api_key: %v", err)
	}
	if rawAPIKey == "sk-ant-legacy-plaintext" {
		t.Fatalf("cloud_providers.api_key still stored as plaintext after migration")
	}
	if !strings.HasPrefix(rawAPIKey, secretEncPrefix) {
		t.Fatalf("cloud_providers.api_key = %q, want %s prefix after migration", rawAPIKey, secretEncPrefix)
	}

	var rawKey string
	if err := ss2.db.QueryRow(`SELECT key FROM runtime_keys WHERE name=?`, "team-a").Scan(&rawKey); err != nil {
		t.Fatalf("select raw key: %v", err)
	}
	if !strings.HasPrefix(rawKey, secretEncPrefix) {
		t.Fatalf("runtime_keys.key = %q, want %s prefix after migration", rawKey, secretEncPrefix)
	}

	var rawSetting string
	if err := ss2.db.QueryRow(`SELECT value FROM settings WHERE key=?`, "litellm_api_key").Scan(&rawSetting); err != nil {
		t.Fatalf("select raw setting: %v", err)
	}
	if !strings.HasPrefix(rawSetting, secretEncPrefix) {
		t.Fatalf("settings litellm_api_key = %q, want %s prefix after migration", rawSetting, secretEncPrefix)
	}

	// Every read path must still hand back the original plaintext.
	providers, err := st2.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders: %v", err)
	}
	if len(providers) != 1 || providers[0].APIKey != "sk-ant-legacy-plaintext" {
		t.Fatalf("AllCloudProviders = %+v, want decrypted plaintext api key", providers)
	}

	keys, err := st2.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Key != "sk-marbor-legacy-plaintext" {
		t.Fatalf("AllKeys = %+v, want decrypted plaintext key", keys)
	}

	gotSetting, err := st2.GetSetting("litellm_api_key")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if gotSetting != "sk-litellm-legacy-plaintext" {
		t.Fatalf("GetSetting(litellm_api_key) = %q, want decrypted plaintext", gotSetting)
	}

	// Re-running migrate again (simulating a third boot) must be a no-op -
	// idempotency guard against double-encryption corrupting the value.
	if err := ss2.migrateEncryptSecrets(); err != nil {
		t.Fatalf("second migrateEncryptSecrets: %v", err)
	}
	gotSetting2, err := st2.GetSetting("litellm_api_key")
	if err != nil {
		t.Fatalf("GetSetting after second migrate: %v", err)
	}
	if gotSetting2 != "sk-litellm-legacy-plaintext" {
		t.Fatalf("GetSetting(litellm_api_key) after second migrate = %q, want unchanged plaintext round-trip", gotSetting2)
	}
}

// TestUpsertCloudProviderEncryptsAtRest verifies the normal write path (not
// the legacy-migration path) stores ciphertext on disk while the Store
// interface keeps returning plaintext to callers.
func TestUpsertCloudProviderEncryptsAtRest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertCloudProvider(CloudProviderRecord{
		Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-openai-real",
	}); err != nil {
		t.Fatalf("UpsertCloudProvider: %v", err)
	}

	ss := st.(*sqliteStore)
	var raw string
	if err := ss.db.QueryRow(`SELECT api_key FROM cloud_providers WHERE name=?`, "openai").Scan(&raw); err != nil {
		t.Fatalf("select raw api_key: %v", err)
	}
	if raw == "sk-openai-real" || !strings.HasPrefix(raw, secretEncPrefix) {
		t.Fatalf("cloud_providers.api_key stored as %q, want encrypted", raw)
	}

	providers, err := st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders: %v", err)
	}
	if len(providers) != 1 || providers[0].APIKey != "sk-openai-real" {
		t.Fatalf("AllCloudProviders = %+v, want decrypted plaintext", providers)
	}
}

// TestAllKeysDropsUndecryptableRowWithoutBreakingOthers is the regression
// guard for the bug this feature almost shipped with: a single corrupt/
// undecryptable runtime_keys.key value must not (a) fail AllKeys() entirely
// (which would zero out every API key at auth.go's boot/reload load, per
// main.go:283's "if err == nil" pattern - a total auth outage from one bad
// row) and must not (b) surface as Key="" (auth.go's key map is keyed by the
// literal string, and "" is trivially reachable via
// "Authorization: Bearer " with a trailing space and no token - that would
// let anyone authenticate as the broken key). The row must simply be absent
// from the result.
func TestAllKeysDropsUndecryptableRowWithoutBreakingOthers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertKey(KeyRecord{Name: "good-key", Key: "sk-marbor-good"}); err != nil {
		t.Fatalf("UpsertKey(good-key): %v", err)
	}

	ss := st.(*sqliteStore)
	// Insert a row whose "key" is neither legacy plaintext nor valid
	// ciphertext under the store's key - simulates disk corruption or a
	// rotated/lost encryption key.
	if _, err := ss.db.Exec(`INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"broken-key", secretEncPrefix+"not-valid-base64-ciphertext!!", 0, 0, 0, "[]", 0); err != nil {
		t.Fatalf("seed broken row: %v", err)
	}

	keys, err := st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: want no error from one corrupt row (would zero out every key at auth.go boot), got %v", err)
	}
	if len(keys) != 1 || keys[0].Name != "good-key" || keys[0].Key != "sk-marbor-good" {
		t.Fatalf("AllKeys = %+v, want only the good row, decrypted, and the broken row absent", keys)
	}
	for _, k := range keys {
		if k.Key == "" {
			t.Fatalf("AllKeys returned a row with Key=\"\" (name=%s) - this would match an empty-token Authorization header and authenticate as that key", k.Name)
		}
	}
}

// TestUpsertKeyRejectsEmptyKey is the regression guard against empty-key
// persistence: encryptSecret/decryptSecret both treat "" as "unset" and
// round-trip it with no error, so an empty runtime_keys.key would otherwise
// persist silently and reappear from AllKeys as Key="" - the same auth-bypass
// shape as TestAllKeysDropsUndecryptableRowWithoutBreakingOthers, but via a
// clean round-trip instead of a decrypt failure. UpsertKey must refuse to
// write the row at all.
func TestUpsertKeyRejectsEmptyKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertKey(KeyRecord{Name: "empty-key", Key: ""}); err == nil {
		t.Fatal("UpsertKey with Key=\"\" want error, got nil - an empty key would authenticate any request with an empty bearer token")
	}

	keys, err := st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("AllKeys = %+v, want no rows persisted for a rejected empty key", keys)
	}
}

// TestAllKeysDropsLegacyEmptyKeyRow covers a row written before
// UpsertKey's empty-key guard existed (or restored from an old backup, or
// edited directly in the DB): its key column round-trips cleanly through
// decryptSecret to "" with no error, so it needs its own drop check in
// AllKeys distinct from the decrypt-failure path.
func TestAllKeysDropsLegacyEmptyKeyRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertKey(KeyRecord{Name: "good-key", Key: "sk-marbor-good"}); err != nil {
		t.Fatalf("UpsertKey(good-key): %v", err)
	}

	ss := st.(*sqliteStore)
	if _, err := ss.db.Exec(`INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"legacy-empty-key", "", 0, 0, 0, "[]", 0); err != nil {
		t.Fatalf("seed legacy empty-key row: %v", err)
	}

	keys, err := st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: want no error from one empty-key row, got %v", err)
	}
	if len(keys) != 1 || keys[0].Name != "good-key" {
		t.Fatalf("AllKeys = %+v, want only the good row, legacy-empty-key row absent", keys)
	}
	for _, k := range keys {
		if k.Key == "" {
			t.Fatalf("AllKeys returned a row with Key=\"\" (name=%s)", k.Name)
		}
	}
}

// TestAllCloudProvidersDropsUndecryptableRow mirrors
// TestAllKeysDropsUndecryptableRowWithoutBreakingOthers for cloud providers.
func TestAllCloudProvidersDropsUndecryptableRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertCloudProvider(CloudProviderRecord{
		Name: "good", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-good",
	}); err != nil {
		t.Fatalf("UpsertCloudProvider(good): %v", err)
	}

	ss := st.(*sqliteStore)
	if _, err := ss.db.Exec(`INSERT INTO cloud_providers (name, provider, base_url, api_key, enabled) VALUES (?, ?, ?, ?, ?)`,
		"broken", "openai", "https://api.openai.com", secretEncPrefix+"not-valid-base64-ciphertext!!", 1); err != nil {
		t.Fatalf("seed broken row: %v", err)
	}

	providers, err := st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders: want no error from one corrupt row, got %v", err)
	}
	if len(providers) != 1 || providers[0].Name != "good" || providers[0].APIKey != "sk-good" {
		t.Fatalf("AllCloudProviders = %+v, want only the good row, decrypted, and the broken row absent", providers)
	}
}
