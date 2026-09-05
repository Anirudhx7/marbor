package store_test

// V1-RELEASE-GATE B.3: "Schema migration proven from every prior released
// version." schema_version_test.go only proves that CurrentSchemaVersion
// (which has never incremented past 1) gets stamped and that a *newer*
// schema_version is refused - it never exercises migrating an *older* real
// DB shape forward. That is what this file does.
//
// migrate() has no versioned branching to iterate (schema_version has only
// ever been 1) - it is unconditional, idempotent DDL: CREATE TABLE IF NOT
// EXISTS for every table, ALTER TABLE ADD COLUMN tolerated for "duplicate
// column name" for every additive column. So "a DB from release X migrates
// cleanly" can only mean: reconstruct what a real marbor.db produced by
// release X's own migrate() actually looked like on disk, seed it with
// representative data, run *today's* migrate() (via the real store.Open()
// boot path) against it, and assert data survives with sane new-column
// defaults. That is what the fixtures in testdata/schema_history/ and the
// tests below do - see that directory's README.md for exactly how each
// fixture was extracted and why each historical point was chosen.

import (
	"database/sql"
	"embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"

	_ "modernc.org/sqlite"
)

//go:embed testdata/schema_history/*.sql
var historicalSchemaFS embed.FS

// tableExists reports whether name is a real table in db (used to skip
// seeding data into tables that did not exist yet at a given historical
// point, e.g. cloud_providers before v0.14.0).
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("tableExists(%s): %v", name, err)
	}
	return n > 0
}

// loadHistoricalDDL parses one embedded fixture into its ordered list of raw
// SQL statements, split on the @@STMT@@ delimiter the extraction tool
// emitted (see testdata/schema_history/README.md).
func loadHistoricalDDL(t *testing.T, file string) []string {
	t.Helper()
	raw, err := historicalSchemaFS.ReadFile("testdata/schema_history/" + file)
	if err != nil {
		t.Fatalf("read fixture %s: %v", file, err)
	}
	var stmts []string
	for _, part := range strings.Split(string(raw), "@@STMT@@\n") {
		part = strings.TrimRight(part, "\n")
		if strings.TrimSpace(part) == "" {
			continue
		}
		stmts = append(stmts, part)
	}
	return stmts
}

// buildHistoricalDB creates a fresh SQLite file at path and applies exactly
// the DDL statement list a real binary at some historical tag ran against
// it, in that tag's own order - i.e. the actual shape that tag's own
// migrate() produced, not a guess at what it might have looked like.
func buildHistoricalDB(t *testing.T, path string, stmts []string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw historical db: %v", err)
	}
	defer db.Close()
	for i, stmt := range stmts {
		// A historical migrate()'s own statement list can name the same
		// column twice (e.g. a CREATE TABLE already carrying a column that
		// a later ALTER TABLE ADD COLUMN in the same file also adds) - the
		// same harmless idempotency the CURRENT migrate() tolerates for
		// exactly this reason (sqlite.go's own ALTER loop). Reconstructing
		// one historical shape from scratch hits this immediately, since
		// nothing here is "pre-existing" the way a real upgrade's DB is.
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			t.Fatalf("historical DDL statement %d failed: %v\nSQL: %s", i, err, stmt)
		}
	}
}

// TestMigrateFromHistoricalSchemas is B.3's evidence: for each historical
// release point, reconstruct that release's real on-disk shape, seed
// representative data using only the columns that existed at that point,
// run the CURRENT migrate() against it via the real store.Open() boot path,
// and assert (a) no error, (b) every seeded row survives with its original
// values, (c) every table/column added after that point now exists with the
// documented default.
func TestMigrateFromHistoricalSchemas(t *testing.T) {
	points := []string{"v0.11.0", "v0.14.0", "v0.16.0", "v0.19.0", "v0.20.0"}

	for _, tag := range points {
		tag := tag
		t.Run(tag, func(t *testing.T) {
			stmts := loadHistoricalDDL(t, tag+".sql")
			if len(stmts) == 0 {
				t.Fatalf("fixture %s.sql produced zero statements", tag)
			}

			dbPath := filepath.Join(t.TempDir(), "marbor.db")
			buildHistoricalDB(t, dbPath, stmts)

			raw, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatalf("reopen raw db to seed: %v", err)
			}
			seedHistoricalData(t, raw, tag)
			if err := raw.Close(); err != nil {
				t.Fatalf("close seeding handle: %v", err)
			}

			s, err := store.Open(dbPath)
			if err != nil {
				t.Fatalf("store.Open() on reconstructed %s shape: %v", tag, err)
			}
			defer s.Close()

			assertSurvival(t, s, tag, dbPath)
		})
	}
}

// seedHistoricalData inserts representative rows using only the columns
// that actually existed in the given historical tag's shape (verified
// against testdata/schema_history/<tag>.sql - see that file's own CREATE
// TABLE / ALTER TABLE list for what is and isn't present at each point).
// Every table used here (runtime_nodes, node_overrides, node_drain,
// runtime_keys) has existed, with at least these columns, since v0.11.0 -
// the earliest point tested - so the same insert works unmodified across
// v0.11.0 through v0.20.0.
func seedHistoricalData(t *testing.T, db *sql.DB, tag string) {
	t.Helper()

	mustExec(t, db,
		`INSERT INTO runtime_nodes (name, url, runtime, vram_total_mb) VALUES (?, ?, ?, ?)`,
		"hist-node", "http://10.0.0.5:11434", "ollama", 24000)
	mustExec(t, db,
		`INSERT INTO node_overrides (name, vram_total_mb, gpu_model) VALUES (?, ?, ?)`,
		"hist-node", 24000, "RTX 4090")
	mustExec(t, db,
		`INSERT INTO node_drain (name, draining) VALUES (?, ?)`,
		"hist-node", 0)

	// runtime_keys.key is seeded as PLAINTEXT for the three tags that predate
	// secretbox (commit fe3437e, 2026-07-16): v0.11.0, v0.14.0, v0.16.0. That
	// is the genuinely faithful historical state - a real DB from any of
	// those releases could only ever have plaintext there, since the
	// encryption feature did not exist yet. v0.19.0/v0.20.0 seed a different
	// plaintext value instead of reusing the pre-secretbox one, to keep the
	// preSecretbox assertion below (which checks for the exact pre-secretbox
	// value) meaningful; migrateEncryptSecrets picks up any plaintext it
	// finds regardless of tag, so this still proves the row (and its
	// non-default fields) survive migration. It is never seeded empty: an
	// empty runtime_keys.key is not a state a real row is ever in (F-C2-01,
	// C.2 security review) - AllKeys() now deliberately drops such a row as
	// an auth-bypass guard (a client sending "Authorization: Bearer " with
	// no token must never match a stored key), so seeding "" here would make
	// this test assert the wrong thing about that guard rather than about
	// schema migration.
	preSecretbox := tag == "v0.11.0" || tag == "v0.14.0" || tag == "v0.16.0"
	keyValue := "sk-hist-post-secretbox-key"
	if preSecretbox {
		keyValue = "sk-hist-plaintext-key"
	}
	mustExec(t, db,
		`INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"hist-key", keyValue, 100, 1000, 30000, `["llama3"]`, 0)

	if tableExists(t, db, "cloud_providers") {
		apiKey := ""
		if preSecretbox {
			apiKey = "sk-hist-plaintext-provider-key"
		}
		mustExec(t, db,
			`INSERT INTO cloud_providers (name, provider, base_url, api_key) VALUES (?, ?, ?, ?)`,
			"hist-provider", "openai", "https://api.openai.com/v1", apiKey)
	}

	// node_agent (v0.19.0's name for what v0.20.0+ calls marbor_agent) is the
	// specific table B.3's audit found a real migration bug in: the v0.20.0
	// rename (commit 19147ea) only renamed the Go string literal, never the
	// table on an existing marbor.db, so a real node_agent row would have
	// been silently orphaned - see migrateRenameNodeAgentTable's doc comment
	// in sqlite.go. Seeded here (v0.19.0's own shape only has 5 columns -
	// name/enabled/port/token/scope; scheme was added between v0.19.0 and
	// the rename) to prove the fix copies it into marbor_agent correctly.
	if tableExists(t, db, "node_agent") {
		mustExec(t, db,
			`INSERT INTO node_agent (name, enabled, port, token, scope) VALUES (?, ?, ?, ?, ?)`,
			"hist-node", 1, 9200, "sk-hist-plaintext-agent-token", "admin")
	}

	// v0.20.0 already has the renamed marbor_agent table directly (no legacy
	// node_agent row to migrate at this point) and a benchmark_runs table
	// that predates the p95/p99/TPOT columns - seeding a row here proves
	// those later-added nullable columns get NULL, never a fabricated 0,
	// on a pre-existing row.
	if tag == "v0.20.0" {
		mustExec(t, db,
			`INSERT INTO benchmark_runs (node, model, n, cold_p50_ms, cold_min_ms, cold_max_ms, warm_p50_ms, warm_min_ms, warm_max_ms, speedup_x, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"hist-node", "llama3", 5, 120.0, 100.0, 150.0, 20.0, 15.0, 30.0, 6.0, 1735689600)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("seed exec failed: %v\nSQL: %s", err, query)
	}
}

// assertSurvival runs the shared checks against the now-current-migrated
// store: every row seedHistoricalData inserted is still present with its
// original values, and every column/table added to migrate() after this
// historical point now exists with the documented default rather than
// erroring, vanishing, or fabricating a non-empty value. dbPath is
// passed only so the v0.20.0 benchmark_runs check (no exported getter
// exists for that table) can re-open the same file read-only, after
// store.Open() has already migrated it.
func assertSurvival(t *testing.T, s store.Store, tag string, dbPath string) {
	t.Helper()

	nodes, err := s.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "hist-node" || nodes[0].URL != "http://10.0.0.5:11434" || nodes[0].Runtime != "ollama" {
		t.Fatalf("runtime_nodes did not survive migration: %+v", nodes)
	}
	if nodes[0].VRAMTotalMB == nil || *nodes[0].VRAMTotalMB != 24000 {
		t.Fatalf("runtime_nodes.vram_total_mb did not survive: %+v", nodes[0])
	}
	// host was added after v0.11.0 - a pre-existing row must get "" (NULL),
	// never a fabricated hostname.
	if nodes[0].Host != "" {
		t.Fatalf("runtime_nodes.host on a pre-existing row should default to empty, got %q", nodes[0].Host)
	}

	overrides, err := s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov, ok := overrides["hist-node"]
	if !ok {
		t.Fatalf("node_overrides row for hist-node did not survive migration")
	}
	if ov.VRAMTotalMB == nil || *ov.VRAMTotalMB != 24000 {
		t.Fatalf("node_overrides.vram_total_mb did not survive: %+v", ov)
	}
	if ov.GPUModel == nil || *ov.GPUModel != "RTX 4090" {
		t.Fatalf("node_overrides.gpu_model did not survive: %+v", ov)
	}
	// Every one of these was added after v0.11.0 (the earliest point this
	// test reconstructs) - a pre-existing row must show "nothing declared"
	// (nil), never a fabricated value, regardless of which historical point
	// this run started from.
	if ov.Runtime != nil || ov.GPUIndices != nil || ov.MaxInFlight != nil ||
		ov.TLSFingerprint != nil || ov.ParallelismType != nil || ov.ParallelismWidth != nil || ov.VRAMOverrides != nil {
		t.Fatalf("node_overrides gained a non-nil later-added field on a pre-existing row: %+v", ov)
	}

	drains, err := s.NodeDrainStates()
	if err != nil {
		t.Fatalf("NodeDrainStates: %v", err)
	}
	drain, ok := drains["hist-node"]
	if !ok {
		t.Fatalf("node_drain row for hist-node did not survive migration")
	}
	if drain.Draining {
		t.Fatalf("node_drain.draining changed value across migration: %+v", drain)
	}
	if drain.Reason != "" {
		t.Fatalf("node_drain.drained_reason on a pre-existing row should default to empty, got %q", drain.Reason)
	}

	keys, err := s.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	var key *store.KeyRecord
	for i := range keys {
		if keys[i].Name == "hist-key" {
			key = &keys[i]
		}
	}
	if key == nil {
		t.Fatalf("runtime_keys row for hist-key did not survive migration (list: %+v)", keys)
	}
	preSecretbox := tag == "v0.11.0" || tag == "v0.14.0" || tag == "v0.16.0"
	if preSecretbox {
		// The whole point of this historical point: migrateEncryptSecrets
		// must have picked up the plaintext seed and encrypted it, and
		// AllKeys() must transparently decrypt it back to the exact
		// original value - proving the plaintext-to-ciphertext upgrade
		// path survives a real historical DB shape, not just a
		// same-version round-trip.
		if key.Key != "sk-hist-plaintext-key" {
			t.Fatalf("runtime_keys.key did not survive the plaintext->encrypted upgrade: got %q", key.Key)
		}
	}
	if key.DailyUsdCap != 0 || key.MonthlyUsdCap != 0 || key.ExpiresAt != "" || key.LocalOnly || key.AllowLocalDegradation {
		t.Fatalf("runtime_keys gained a non-default later-added field on a pre-existing row: %+v", key)
	}

	providers, err := s.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders: %v", err)
	}
	for _, cp := range providers {
		if cp.Name != "hist-provider" {
			continue
		}
		if preSecretbox && cp.APIKey != "sk-hist-plaintext-provider-key" {
			t.Fatalf("cloud_providers.api_key did not survive the plaintext->encrypted upgrade: got %q", cp.APIKey)
		}
	}

	if tag == "v0.19.0" {
		// The bug this test found: the v0.20.0 rename (node_agent ->
		// marbor_agent) never migrated existing rows. Without
		// migrateRenameNodeAgentTable, GetMarborAgent here would return
		// found=false and the seeded enrollment would be silently gone.
		//
		// The row is looked up by "10.0.0.5", not "hist-node": the
		// pre-existing (unrelated to this fix) migrateMarborAgentRekeyByHost
		// runs right after the rename and moves any marbor_agent row still
		// keyed by a node name onto that node's URL-derived host key - here
		// hist-node's URL is http://10.0.0.5:11434, so it re-keys to
		// "10.0.0.5". That is correct, intentional behavior of an existing
		// migration, not something this fix changes.
		rec, found, err := s.GetMarborAgent("10.0.0.5")
		if err != nil {
			t.Fatalf("GetMarborAgent after node_agent->marbor_agent migration: %v", err)
		}
		if !found {
			t.Fatalf("legacy node_agent row for hist-node was lost across the rename to marbor_agent")
		}
		if !rec.Enabled || rec.Port != 9200 || rec.Token != "sk-hist-plaintext-agent-token" || rec.Scope != "admin" {
			t.Fatalf("marbor_agent row migrated from legacy node_agent has wrong values: %+v", rec)
		}
		if rec.Scheme != "http" {
			t.Fatalf("marbor_agent.scheme on a row migrated from a pre-scheme node_agent should default to 'http', got %q", rec.Scheme)
		}
	}

	if tag == "v0.20.0" {
		// v0.20.0 has no exported getter for benchmark_runs, so this one
		// assertion reaches the raw file directly (read-only, after
		// store.Open() has already migrated it) purely to check the six
		// six latency-tail-and-TPOT columns' default - proving a pre-existing row gets NULL
		// ("not computed"), never a fabricated 0, not to bypass the
		// Store API for anything mutable.
		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("reopen db for benchmark_runs check: %v", err)
		}
		defer raw.Close()
		var coldP95, coldP99, warmP95, warmP99, coldTPOT, warmTPOT sql.NullFloat64
		err = raw.QueryRow(
			`SELECT cold_p95_ms, cold_p99_ms, warm_p95_ms, warm_p99_ms, cold_tpot_p50_ms, warm_tpot_p50_ms
			 FROM benchmark_runs WHERE node = 'hist-node' AND model = 'llama3'`,
		).Scan(&coldP95, &coldP99, &warmP95, &warmP99, &coldTPOT, &warmTPOT)
		if err != nil {
			t.Fatalf("query pre-existing benchmark_runs row: %v", err)
		}
		if coldP95.Valid || coldP99.Valid || warmP95.Valid || warmP99.Valid || coldTPOT.Valid || warmTPOT.Valid {
			t.Fatalf("benchmark_runs latency-tail-and-TPOT columns should be NULL on a pre-existing row, got p95=%v p99=%v warm_p95=%v warm_p99=%v cold_tpot=%v warm_tpot=%v",
				coldP95, coldP99, warmP95, warmP99, coldTPOT, warmTPOT)
		}
	}
}
