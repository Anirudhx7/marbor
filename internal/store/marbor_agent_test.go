package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarborAgentRecord_TokenNeverMarshaled(t *testing.T) {
	rec := MarborAgentRecord{Name: "gpu-1", Enabled: true, Port: 9200, Token: "secret-value"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), "secret-value") {
		t.Fatalf("MarborAgentRecord must never marshal Token (P68 - closes config-dump leak path), got %s", b)
	}
}

func TestUserSession_TokenNeverMarshaled(t *testing.T) {
	sess := UserSession{Token: "secret-session-token", UserID: 1, Role: "admin", Username: "u"}
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), "secret-session-token") {
		t.Fatalf("UserSession must never marshal Token, got %s", b)
	}
	if strings.Contains(string(b), `"token"`) {
		t.Fatalf("UserSession must never marshal a token key, got %s", b)
	}
}

func TestUpsertAndGetMarborAgent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rec := MarborAgentRecord{Name: "gpu-1", Enabled: true, Port: 9200, Token: "sk-agent-token-real"}
	if err := st.UpsertMarborAgent(rec); err != nil {
		t.Fatalf("UpsertMarborAgent: %v", err)
	}

	got, found, err := st.GetMarborAgent("gpu-1")
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if !found {
		t.Fatal("GetMarborAgent: found=false, want true")
	}
	if got.Name != "gpu-1" || !got.Enabled || got.Port != 9200 || got.Token != "sk-agent-token-real" {
		t.Fatalf("GetMarborAgent = %+v, want round-tripped plaintext record", got)
	}
}

// TestUpsertAndGetMarborAgent_Scope is the P54 store-level round-trip: Scope
// persists alongside Token and comes back unchanged through GetMarborAgent.
func TestUpsertAndGetMarborAgent_Scope(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	rec := MarborAgentRecord{Name: "gpu-1", Enabled: true, Port: 9200, Token: "operator.sk-agent-token", Scope: "operator"}
	if err := st.UpsertMarborAgent(rec); err != nil {
		t.Fatalf("UpsertMarborAgent: %v", err)
	}

	got, found, err := st.GetMarborAgent("gpu-1")
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if !found {
		t.Fatal("GetMarborAgent: found=false, want true")
	}
	if got.Scope != "operator" {
		t.Fatalf("GetMarborAgent.Scope = %q, want %q", got.Scope, "operator")
	}
}

// TestMarborAgentRowPredatingScopeColumnDefaultsToAdmin verifies a row
// inserted before the P54 scope column existed (the exact shape of an
// existing installation's marbor.db before this migration ran) reads back as
// "admin" - matching that row's actual token, which has no scope prefix and
// so parses as tierAdmin via scopeOf's fallback (backward compatible).
func TestMarborAgentRowPredatingScopeColumnDefaultsToAdmin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ss := st.(*sqliteStore)
	// Deliberately omit the scope column, simulating a pre-P54 row (or the
	// ALTER TABLE migration's default applying to a row written before it
	// ran).
	if _, err := ss.db.Exec(`INSERT INTO marbor_agent (name, enabled, port, token) VALUES (?, ?, ?, ?)`,
		"legacy-node", 1, 9200, "legacy-plaintext-token"); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	got, found, err := st.GetMarborAgent("legacy-node")
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if !found {
		t.Fatal("GetMarborAgent: found=false, want true")
	}
	if got.Scope != "admin" {
		t.Fatalf("GetMarborAgent.Scope for a pre-P54 row = %q, want %q (column default)", got.Scope, "admin")
	}
}

// TestMigrateMarborAgentRekeyByHostPreservesScopeAndScheme is a regression
// test for B1 finding STORE-06: migrateMarborAgentRekeyByHost's rekey INSERT
// used to name only (name, enabled, port, token), silently dropping scope
// and scheme when a legacy-name-keyed row was moved onto its node's shared
// host key. On a fleet upgrading from v0.19.x with a readonly-scoped or
// https-scheme row, that omission meant the migrated row silently fell back
// to the column defaults ('admin' scope, 'http' scheme) - a privilege
// escalation and a plaintext downgrade, both irreversible once the legacy
// row was deleted by the same migration.
func TestMigrateMarborAgentRekeyByHostPreservesScopeAndScheme(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ss := st.(*sqliteStore)

	const legacyName = "old-node-name"
	const sharedHost = "gpu-host-1"
	if err := st.UpsertNode(NodeRecord{Name: legacyName, URL: "http://10.0.0.5:11434", Runtime: "ollama", Host: sharedHost}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	// Seed a row keyed by the pre-host-scoping legacy node name, with a
	// non-default scope and scheme - exactly the shape a v0.19.x install
	// would have on disk.
	if _, err := ss.db.Exec(
		`INSERT INTO marbor_agent (name, enabled, port, token, scope, scheme) VALUES (?, ?, ?, ?, ?, ?)`,
		legacyName, 1, 9200, "readonly.sk-legacy-token", "readonly", "https",
	); err != nil {
		t.Fatalf("seed legacy-keyed row: %v", err)
	}

	if err := ss.migrateMarborAgentRekeyByHost(); err != nil {
		t.Fatalf("migrateMarborAgentRekeyByHost: %v", err)
	}

	got, found, err := st.GetMarborAgent(sharedHost)
	if err != nil {
		t.Fatalf("GetMarborAgent(%q): %v", sharedHost, err)
	}
	if !found {
		t.Fatalf("GetMarborAgent(%q): found=false, want true after rekey", sharedHost)
	}
	if got.Scope != "readonly" {
		t.Fatalf("rekeyed row Scope = %q, want %q (must not silently escalate to admin)", got.Scope, "readonly")
	}
	if got.Scheme != "https" {
		t.Fatalf("rekeyed row Scheme = %q, want %q (must not silently downgrade to http)", got.Scheme, "https")
	}

	if _, found, err := st.GetMarborAgent(legacyName); err != nil {
		t.Fatalf("GetMarborAgent(%q): %v", legacyName, err)
	} else if found {
		t.Fatalf("legacy-keyed row %q should have been deleted by the rekey migration", legacyName)
	}
}

func TestGetMarborAgentNotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	_, found, err := st.GetMarborAgent("nonexistent")
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if found {
		t.Fatal("GetMarborAgent: found=true for a node with no agent row, want false")
	}
}

// TestMarborAgentTokenEncryptedAtRest verifies the token is stored under the
// enc:v1: prefix on disk, not as plaintext (R8).
func TestMarborAgentTokenEncryptedAtRest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "gpu-1", Enabled: true, Port: 9200, Token: "sk-agent-token-real"}); err != nil {
		t.Fatalf("UpsertMarborAgent: %v", err)
	}

	ss := st.(*sqliteStore)
	var raw string
	if err := ss.db.QueryRow(`SELECT token FROM marbor_agent WHERE name=?`, "gpu-1").Scan(&raw); err != nil {
		t.Fatalf("select raw token: %v", err)
	}
	if raw == "sk-agent-token-real" || !strings.HasPrefix(raw, secretEncPrefix) {
		t.Fatalf("marbor_agent.token stored as %q, want enc:v1:-prefixed ciphertext", raw)
	}
}

func TestAllMarborAgents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "gpu-1", Enabled: true, Port: 9200, Token: "tok-1"}); err != nil {
		t.Fatalf("UpsertMarborAgent(gpu-1): %v", err)
	}
	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "gpu-2", Enabled: false, Port: 9201, Token: "tok-2"}); err != nil {
		t.Fatalf("UpsertMarborAgent(gpu-2): %v", err)
	}

	all, err := st.AllMarborAgents()
	if err != nil {
		t.Fatalf("AllMarborAgents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllMarborAgents returned %d rows, want 2", len(all))
	}
	byName := make(map[string]MarborAgentRecord, len(all))
	for _, r := range all {
		byName[r.Name] = r
	}
	if !byName["gpu-1"].Enabled || byName["gpu-1"].Token != "tok-1" || byName["gpu-1"].Port != 9200 {
		t.Fatalf("gpu-1 = %+v, want enabled/tok-1/9200", byName["gpu-1"])
	}
	if byName["gpu-2"].Enabled || byName["gpu-2"].Token != "tok-2" {
		t.Fatalf("gpu-2 = %+v, want disabled/tok-2", byName["gpu-2"])
	}
}

// TestAllMarborAgentsDropsUndecryptableRow mirrors
// TestAllKeysDropsUndecryptableRowWithoutBreakingOthers (secretbox_test.go):
// one corrupt/undecryptable marbor_agent.token must not fail the whole list -
// this feeds the router's boot-time agent poll wiring, and one bad row must
// not blank out telemetry for every other node.
func TestAllMarborAgentsDropsUndecryptableRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "good", Enabled: true, Port: 9200, Token: "tok-good"}); err != nil {
		t.Fatalf("UpsertMarborAgent(good): %v", err)
	}

	ss := st.(*sqliteStore)
	if _, err := ss.db.Exec(`INSERT INTO marbor_agent (name, enabled, port, token) VALUES (?, ?, ?, ?)`,
		"broken", 1, 9201, secretEncPrefix+"not-valid-base64-ciphertext!!"); err != nil {
		t.Fatalf("seed broken row: %v", err)
	}

	all, err := st.AllMarborAgents()
	if err != nil {
		t.Fatalf("AllMarborAgents: want no error from one corrupt row, got %v", err)
	}
	if len(all) != 1 || all[0].Name != "good" || all[0].Token != "tok-good" {
		t.Fatalf("AllMarborAgents = %+v, want only the good row, decrypted, broken row absent", all)
	}
	for _, r := range all {
		if r.Token == "" {
			t.Fatalf("AllMarborAgents returned a row with Token=\"\" (name=%s); an empty token must never authenticate", r.Name)
		}
	}
}

func TestDeleteMarborAgent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "gpu-1", Enabled: true, Port: 9200, Token: "tok-1"}); err != nil {
		t.Fatalf("UpsertMarborAgent: %v", err)
	}
	if err := st.DeleteMarborAgent("gpu-1"); err != nil {
		t.Fatalf("DeleteMarborAgent: %v", err)
	}
	_, found, err := st.GetMarborAgent("gpu-1")
	if err != nil {
		t.Fatalf("GetMarborAgent after delete: %v", err)
	}
	if found {
		t.Fatal("GetMarborAgent after DeleteMarborAgent: found=true, want false")
	}
}

// TestDeleteNodeCascadesMarborAgent verifies that removing a node (DeleteNode)
// also removes its marbor_agent row - a stale agent token for a deleted node
// is a dangling-secret concern (R8), even though other per-node aux tables
// (node_overrides, node_drain) are left behind by the existing pattern.
func TestDeleteNodeCascadesMarborAgent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// marbor_agent is keyed by the node's shared host, not its name (see
	// admin.go's handleEnableMarborAgent: MarborAgentRecord{Name: host, ...}) -
	// this fixture mirrors that real write pattern.
	if err := st.UpsertNode(NodeRecord{Name: "gpu-1", URL: "http://10.0.0.5:11434", Runtime: "ollama"}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "10.0.0.5", Enabled: true, Port: 9200, Token: "tok-1"}); err != nil {
		t.Fatalf("UpsertMarborAgent: %v", err)
	}

	if err := st.DeleteNode("gpu-1"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}

	_, found, err := st.GetMarborAgent("10.0.0.5")
	if err != nil {
		t.Fatalf("GetMarborAgent after DeleteNode: %v", err)
	}
	if found {
		t.Fatal("DeleteNode did not cascade-delete the marbor_agent row - stale token left behind (R8)")
	}
}

// TestDeleteNodeDoesNotCascadeSharedHostAgent verifies DeleteNode leaves the
// marbor_agent row alone when another node still shares its host - deleting
// one runtime on a multi-runtime box must not kill the Marbor Agent config for
// its sibling node(s) on the same physical machine.
func TestDeleteNodeDoesNotCascadeSharedHostAgent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNode(NodeRecord{Name: "gpu-1", URL: "http://10.0.0.5:11434", Runtime: "ollama"}); err != nil {
		t.Fatalf("UpsertNode gpu-1: %v", err)
	}
	if err := st.UpsertNode(NodeRecord{Name: "gpu-2", URL: "http://10.0.0.5:8000", Runtime: "vllm"}); err != nil {
		t.Fatalf("UpsertNode gpu-2: %v", err)
	}
	if err := st.UpsertMarborAgent(MarborAgentRecord{Name: "10.0.0.5", Enabled: true, Port: 9200, Token: "tok-1"}); err != nil {
		t.Fatalf("UpsertMarborAgent: %v", err)
	}

	if err := st.DeleteNode("gpu-1"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}

	_, found, err := st.GetMarborAgent("10.0.0.5")
	if err != nil {
		t.Fatalf("GetMarborAgent after DeleteNode: %v", err)
	}
	if !found {
		t.Fatal("DeleteNode wrongly cascade-deleted the marbor_agent row still used by gpu-2")
	}
}

// TestMigrateEncryptSecretsUpgradesLegacyMarborAgentToken mirrors
// TestMigrateEncryptSecretsUpgradesLegacyPlaintext (secretbox_test.go) for
// the marbor_agent table: a plaintext token written before this feature
// existed (or by some other bypass) must be encrypted in place on the next
// boot, transparently, with no manual step.
func TestMigrateEncryptSecretsUpgradesLegacyMarborAgentToken(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := st.(*sqliteStore)
	if _, err := ss.db.Exec(`INSERT INTO marbor_agent (name, enabled, port, token) VALUES (?, ?, ?, ?)`,
		"gpu-1", 1, 9200, "legacy-plaintext-token"); err != nil {
		t.Fatalf("seed marbor_agent: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	defer st2.Close()
	ss2 := st2.(*sqliteStore)

	var raw string
	if err := ss2.db.QueryRow(`SELECT token FROM marbor_agent WHERE name=?`, "gpu-1").Scan(&raw); err != nil {
		t.Fatalf("select raw token: %v", err)
	}
	if raw == "legacy-plaintext-token" {
		t.Fatal("marbor_agent.token still stored as plaintext after migration")
	}
	if !strings.HasPrefix(raw, secretEncPrefix) {
		t.Fatalf("marbor_agent.token = %q, want enc:v1: prefix after migration", raw)
	}

	rec, found, err := st2.GetMarborAgent("gpu-1")
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if !found || rec.Token != "legacy-plaintext-token" {
		t.Fatalf("GetMarborAgent = %+v, found=%v, want decrypted legacy plaintext", rec, found)
	}
}
