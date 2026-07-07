package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteStore is the SQLite-backed Store implementation.
type sqliteStore struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at path, runs migrations, and
// returns a Store. The driver name is "sqlite" (modernc.org/sqlite, no CGO).
func Open(path string) (Store, error) {
	// Pragmas are encoded in the DSN (not run via db.Exec after Open) because
	// database/sql opens multiple physical connections under the hood; an
	// Exec-based PRAGMA only lands on whichever single connection ran it,
	// leaving the rest at driver defaults (busy_timeout=0), which caused
	// spurious SQLITE_BUSY under write contention. DSN pragmas are applied by
	// modernc.org/sqlite to every connection it opens.
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// WAL mode allows concurrent readers alongside a single writer; give the
	// pool enough connections to actually use that (SQLITE_BUSY on write
	// contention is absorbed by busy_timeout above).
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	s := &sqliteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	// The DB stores secrets (API keys, session tokens, cloud provider keys).
	// By default SQLite creates the file with the process umask (often
	// world-readable 0644). Best-effort tighten to 0600 to match SaveConfig.
	// A failed chmod (e.g. on a filesystem that doesn't support it, or on
	// Windows where os.Chmod only toggles the read-only bit) must NOT break
	// startup, so errors are ignored. The pragmas/migrations above have
	// already forced the file (and WAL sidecars) to exist on disk.
	if path != "" && path != ":memory:" {
		_ = os.Chmod(path, 0o600)
		_ = os.Chmod(path+"-wal", 0o600)
		_ = os.Chmod(path+"-shm", 0o600)
	}

	return s, nil
}

func (s *sqliteStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_log (
			id          TEXT PRIMARY KEY,
			key_name    TEXT,
			model       TEXT,
			node_name   TEXT,
			status_code INTEGER,
			latency_ms  INTEGER,
			tokens_used INTEGER,
			cost_usd    REAL,
			routed_to   TEXT,
			is_cloud    INTEGER,
			ts          INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS request_log_ts ON request_log(ts DESC)`,

		`CREATE TABLE IF NOT EXISTS hourly_buckets (
			hour           INTEGER PRIMARY KEY,
			requests       INTEGER,
			tokens         INTEGER,
			cloud_requests INTEGER,
			local_requests INTEGER,
			cost_usd       REAL
		)`,

		`CREATE TABLE IF NOT EXISTS model_stats (
			model    TEXT PRIMARY KEY,
			requests INTEGER,
			tokens   INTEGER,
			cost_usd REAL
		)`,

		`CREATE TABLE IF NOT EXISTS counters (
			id              INTEGER PRIMARY KEY CHECK(id=1),
			local_requests  INTEGER,
			cloud_requests  INTEGER,
			total_tokens    INTEGER,
			cloud_spent_usd REAL
		)`,

		`CREATE TABLE IF NOT EXISTS key_counters (
			name         TEXT PRIMARY KEY,
			today        INTEGER,
			month        INTEGER,
			tokens_today INTEGER,
			tokens_month INTEGER,
			last_reset   INTEGER
		)`,

		`CREATE TABLE IF NOT EXISTS runtime_nodes (
			name         TEXT PRIMARY KEY,
			url          TEXT,
			runtime      TEXT,
			vram_total_mb INTEGER
		)`,

		`CREATE TABLE IF NOT EXISTS node_overrides (
			name         TEXT PRIMARY KEY,
			vram_total_mb INTEGER,
			gpu_model    TEXT
		)`,

		`CREATE TABLE IF NOT EXISTS node_drain (
			name     TEXT PRIMARY KEY,
			draining INTEGER
		)`,

		`CREATE TABLE IF NOT EXISTS runtime_keys (
			name          TEXT PRIMARY KEY,
			key           TEXT,
			rate_limit    INTEGER,
			daily_limit   INTEGER,
			monthly_limit INTEGER,
			models        TEXT,
			revoked       INTEGER
		)`,

		`CREATE TABLE IF NOT EXISTS audit_log (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			ts          TEXT NOT NULL,
			request_id  TEXT NOT NULL,
			key_name    TEXT NOT NULL,
			model       TEXT NOT NULL,
			node        TEXT NOT NULL,
			status      TEXT NOT NULL,
			latency_ms  INTEGER NOT NULL,
			cloud       INTEGER NOT NULL DEFAULT 0,
			cloud_model TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(ts DESC)`,

		`CREATE TABLE IF NOT EXISTS admin_credentials (
			id            INTEGER PRIMARY KEY CHECK(id=1),
			username      TEXT NOT NULL DEFAULT 'admin',
			password_hash TEXT NOT NULL,
			salt          TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS admin_sessions (
			token      TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS users (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			username             TEXT NOT NULL UNIQUE COLLATE NOCASE,
			email                TEXT NOT NULL DEFAULT '',
			password_hash        TEXT NOT NULL DEFAULT '',
			salt                 TEXT NOT NULL DEFAULT '',
			role                 TEXT NOT NULL DEFAULT 'user'
			                     CHECK(role IN ('admin','user')),
			status               TEXT NOT NULL DEFAULT 'pending'
			                     CHECK(status IN ('pending','active','suspended')),
			api_key_name         TEXT NOT NULL DEFAULT '',
			must_change_password INTEGER NOT NULL DEFAULT 0,
			created_at           INTEGER NOT NULL,
			approved_at          INTEGER,
			approved_by          TEXT NOT NULL DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS user_sessions (
			token                TEXT PRIMARY KEY,
			user_id              INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role                 TEXT NOT NULL,
			username             TEXT NOT NULL,
			must_change_password INTEGER NOT NULL DEFAULT 0,
			created_at           INTEGER NOT NULL,
			expires_at           INTEGER NOT NULL
		)`,

		// --- Phase 1: SQLite-first tables ---

		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS cloud_providers (
			name               TEXT PRIMARY KEY,
			provider           TEXT NOT NULL,
			base_url           TEXT NOT NULL,
			api_key            TEXT NOT NULL DEFAULT '',
			default_model      TEXT NOT NULL DEFAULT '',
			cost_per_1k_tokens REAL NOT NULL DEFAULT 0.0,
			enabled            INTEGER NOT NULL DEFAULT 1,
			priority           INTEGER NOT NULL DEFAULT 0
		)`,

		`CREATE TABLE IF NOT EXISTS routing_rules (
			id         TEXT PRIMARY KEY,
			condition  TEXT NOT NULL,
			target     TEXT NOT NULL,
			priority   INTEGER NOT NULL DEFAULT 0,
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS warmup_config (
			id         INTEGER PRIMARY KEY CHECK(id=1),
			enabled    INTEGER NOT NULL DEFAULT 0,
			keep_alive TEXT NOT NULL DEFAULT '10m'
		)`,

		`CREATE TABLE IF NOT EXISTS warmup_models (
			model      TEXT PRIMARY KEY,
			nodes_json TEXT NOT NULL DEFAULT '[]'
		)`,

		// warm_state is the persisted model residency map (Phase 1: Persistent
		// Warm State). Rows survive a restart so the router starts warm.
		`CREATE TABLE IF NOT EXISTS warm_state (
			model      TEXT NOT NULL,
			node       TEXT NOT NULL,
			last_used  INTEGER NOT NULL DEFAULT 0,
			vram_bytes INTEGER NOT NULL DEFAULT 0,
			load_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (model, node)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate stmt: %w\nSQL: %s", err, stmt)
		}
	}
	// Idempotent column additions for schema upgrades on existing DBs.
	for _, col := range []string{
		`ALTER TABLE users ADD COLUMN deleted_at INTEGER`,
		`ALTER TABLE users ADD COLUMN deleted_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE hourly_buckets ADD COLUMN gen_duration_ms INTEGER NOT NULL DEFAULT 0`,
	} {
		s.db.Exec(col) // ignore error — column may already exist
	}
	return nil
}

// --- Request log ---

func (s *sqliteStore) AppendRequest(r RequestRecord) error {
	isCloud := 0
	if r.IsCloud {
		isCloud = 1
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO request_log
			(id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.KeyName, r.Model, r.NodeName, r.StatusCode, r.LatencyMs, r.TokensUsed, r.CostUSD, r.RoutedTo, isCloud, r.TS.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: AppendRequest: %w", err)
	}
	// Trim to last 1000 rows.
	_, err = s.db.Exec(
		`DELETE FROM request_log WHERE id NOT IN (SELECT id FROM request_log ORDER BY ts DESC LIMIT 1000)`,
	)
	if err != nil {
		return fmt.Errorf("store: AppendRequest trim: %w", err)
	}
	return nil
}

func (s *sqliteStore) LastRequests(n int) ([]RequestRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts
		 FROM request_log ORDER BY ts DESC LIMIT ?`, n,
	)
	if err != nil {
		return nil, fmt.Errorf("store: LastRequests: %w", err)
	}
	defer rows.Close()

	var recs []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var isCloud int
		var ts int64
		if err := rows.Scan(
			&r.ID, &r.KeyName, &r.Model, &r.NodeName, &r.StatusCode,
			&r.LatencyMs, &r.TokensUsed, &r.CostUSD, &r.RoutedTo, &isCloud, &ts,
		); err != nil {
			return nil, fmt.Errorf("store: LastRequests scan: %w", err)
		}
		r.IsCloud = isCloud != 0
		r.TS = time.Unix(ts, 0).UTC()
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: LastRequests rows: %w", err)
	}
	// Reverse to ascending order (oldest first).
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs, nil
}

// --- Analytics ---

// UpsertHourlyBucket ADDS the given counts to the bucket for its hour. Callers
// pass a single request's delta (e.g. local_requests:1); the values accumulate
// per hour so that after a restart restoreFromStore reads real cumulative totals
// rather than the last request's values. (INSERT OR REPLACE previously clobbered
// the row on every request, persisting only the final request's counts.)
func (s *sqliteStore) UpsertHourlyBucket(b HourlyBucket) error {
	hourUnix := b.Hour.Truncate(time.Hour).Unix()
	_, err := s.db.Exec(
		`INSERT INTO hourly_buckets
			(hour, requests, tokens, cloud_requests, local_requests, cost_usd, gen_duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(hour) DO UPDATE SET
			requests        = requests + excluded.requests,
			tokens          = tokens + excluded.tokens,
			cloud_requests  = cloud_requests + excluded.cloud_requests,
			local_requests  = local_requests + excluded.local_requests,
			cost_usd        = cost_usd + excluded.cost_usd,
			gen_duration_ms = gen_duration_ms + excluded.gen_duration_ms`,
		hourUnix, b.Requests, b.Tokens, b.CloudRequests, b.LocalRequests, b.CostUSD, b.GenDurationMs,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertHourlyBucket: %w", err)
	}
	return nil
}

func (s *sqliteStore) HourlyBuckets(since time.Time) ([]HourlyBucket, error) {
	rows, err := s.db.Query(
		`SELECT hour, requests, tokens, cloud_requests, local_requests, cost_usd, gen_duration_ms
		 FROM hourly_buckets WHERE hour >= ? ORDER BY hour ASC`,
		since.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("store: HourlyBuckets: %w", err)
	}
	defer rows.Close()

	var buckets []HourlyBucket
	for rows.Next() {
		var b HourlyBucket
		var hourUnix int64
		if err := rows.Scan(&hourUnix, &b.Requests, &b.Tokens, &b.CloudRequests, &b.LocalRequests, &b.CostUSD, &b.GenDurationMs); err != nil {
			return nil, fmt.Errorf("store: HourlyBuckets scan: %w", err)
		}
		b.Hour = time.Unix(hourUnix, 0).UTC()
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: HourlyBuckets rows: %w", err)
	}
	return buckets, nil
}

// UpsertModelStat ADDS the given counts to the per-model row (same accumulate
// semantics as UpsertHourlyBucket — callers pass a single request's delta).
func (s *sqliteStore) UpsertModelStat(ms ModelStat) error {
	_, err := s.db.Exec(
		`INSERT INTO model_stats (model, requests, tokens, cost_usd)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(model) DO UPDATE SET
			requests = requests + excluded.requests,
			tokens   = tokens + excluded.tokens,
			cost_usd = cost_usd + excluded.cost_usd`,
		ms.Model, ms.Requests, ms.Tokens, ms.CostUSD,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertModelStat: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllModelStats() ([]ModelStat, error) {
	rows, err := s.db.Query(
		`SELECT model, requests, tokens, cost_usd FROM model_stats ORDER BY requests DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllModelStats: %w", err)
	}
	defer rows.Close()

	var stats []ModelStat
	for rows.Next() {
		var ms ModelStat
		if err := rows.Scan(&ms.Model, &ms.Requests, &ms.Tokens, &ms.CostUSD); err != nil {
			return nil, fmt.Errorf("store: AllModelStats scan: %w", err)
		}
		stats = append(stats, ms)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllModelStats rows: %w", err)
	}
	return stats, nil
}

// --- Global counters ---

func (s *sqliteStore) SetCounters(c Counters) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO counters
			(id, local_requests, cloud_requests, total_tokens, cloud_spent_usd)
			VALUES (1, ?, ?, ?, ?)`,
		c.LocalRequests, c.CloudRequests, c.TotalTokens, c.CloudSpentUSD,
	)
	if err != nil {
		return fmt.Errorf("store: SetCounters: %w", err)
	}
	return nil
}

func (s *sqliteStore) GetCounters() (Counters, error) {
	var c Counters
	err := s.db.QueryRow(
		`SELECT local_requests, cloud_requests, total_tokens, cloud_spent_usd
		 FROM counters WHERE id=1`,
	).Scan(&c.LocalRequests, &c.CloudRequests, &c.TotalTokens, &c.CloudSpentUSD)
	if err == sql.ErrNoRows {
		return Counters{}, nil
	}
	if err != nil {
		return Counters{}, fmt.Errorf("store: GetCounters: %w", err)
	}
	return c, nil
}

// --- Per-key counters ---

func (s *sqliteStore) SaveKeyCounters(name string, snap KeyCounterSnapshot) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO key_counters
			(name, today, month, tokens_today, tokens_month, last_reset)
			VALUES (?, ?, ?, ?, ?, ?)`,
		name, snap.Today, snap.Month, snap.TokensToday, snap.TokensMonth, snap.LastReset.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: SaveKeyCounters: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllKeyCounters() (map[string]KeyCounterSnapshot, error) {
	rows, err := s.db.Query(
		`SELECT name, today, month, tokens_today, tokens_month, last_reset FROM key_counters`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllKeyCounters: %w", err)
	}
	defer rows.Close()

	out := make(map[string]KeyCounterSnapshot)
	for rows.Next() {
		var name string
		var snap KeyCounterSnapshot
		var lastReset int64
		if err := rows.Scan(&name, &snap.Today, &snap.Month, &snap.TokensToday, &snap.TokensMonth, &lastReset); err != nil {
			return nil, fmt.Errorf("store: AllKeyCounters scan: %w", err)
		}
		snap.LastReset = time.Unix(lastReset, 0).UTC()
		out[name] = snap
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllKeyCounters rows: %w", err)
	}
	return out, nil
}

// --- Runtime nodes ---

func (s *sqliteStore) UpsertNode(nc NodeRecord) error {
	var vram sql.NullInt64
	if nc.VRAMTotalMB != nil {
		vram = sql.NullInt64{Int64: *nc.VRAMTotalMB, Valid: true}
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO runtime_nodes (name, url, runtime, vram_total_mb)
		 VALUES (?, ?, ?, ?)`,
		nc.Name, nc.URL, nc.Runtime, vram,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertNode: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteNode(name string) error {
	_, err := s.db.Exec(`DELETE FROM runtime_nodes WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("store: DeleteNode: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllNodes() ([]NodeRecord, error) {
	rows, err := s.db.Query(
		`SELECT name, url, runtime, vram_total_mb FROM runtime_nodes`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllNodes: %w", err)
	}
	defer rows.Close()

	var nodes []NodeRecord
	for rows.Next() {
		var nc NodeRecord
		var vram sql.NullInt64
		if err := rows.Scan(&nc.Name, &nc.URL, &nc.Runtime, &vram); err != nil {
			return nil, fmt.Errorf("store: AllNodes scan: %w", err)
		}
		if vram.Valid {
			nc.VRAMTotalMB = &vram.Int64
		}
		nodes = append(nodes, nc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllNodes rows: %w", err)
	}
	return nodes, nil
}

// --- Node overrides ---

func (s *sqliteStore) UpsertNodeOverride(name string, vramTotalMB *int64, gpuModel *string) error {
	var vram sql.NullInt64
	if vramTotalMB != nil {
		vram = sql.NullInt64{Int64: *vramTotalMB, Valid: true}
	}
	var gpu sql.NullString
	if gpuModel != nil {
		gpu = sql.NullString{String: *gpuModel, Valid: true}
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO node_overrides (name, vram_total_mb, gpu_model)
		 VALUES (?, ?, ?)`,
		name, vram, gpu,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertNodeOverride: %w", err)
	}
	return nil
}

func (s *sqliteStore) NodeOverrides() (map[string]NodeOverride, error) {
	rows, err := s.db.Query(
		`SELECT name, vram_total_mb, gpu_model FROM node_overrides`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: NodeOverrides: %w", err)
	}
	defer rows.Close()

	out := make(map[string]NodeOverride)
	for rows.Next() {
		var name string
		var vram sql.NullInt64
		var gpu sql.NullString
		if err := rows.Scan(&name, &vram, &gpu); err != nil {
			return nil, fmt.Errorf("store: NodeOverrides scan: %w", err)
		}
		var ov NodeOverride
		if vram.Valid {
			ov.VRAMTotalMB = &vram.Int64
		}
		if gpu.Valid {
			ov.GPUModel = &gpu.String
		}
		out[name] = ov
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: NodeOverrides rows: %w", err)
	}
	return out, nil
}

// --- Node drain state ---

func (s *sqliteStore) SetNodeDrain(name string, draining bool) error {
	d := 0
	if draining {
		d = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO node_drain (name, draining) VALUES (?, ?)`,
		name, d,
	)
	if err != nil {
		return fmt.Errorf("store: SetNodeDrain: %w", err)
	}
	return nil
}

func (s *sqliteStore) NodeDrainStates() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name, draining FROM node_drain`)
	if err != nil {
		return nil, fmt.Errorf("store: NodeDrainStates: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var name string
		var d int
		if err := rows.Scan(&name, &d); err != nil {
			return nil, fmt.Errorf("store: NodeDrainStates scan: %w", err)
		}
		out[name] = d != 0
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: NodeDrainStates rows: %w", err)
	}
	return out, nil
}

// --- Runtime API keys ---

func (s *sqliteStore) UpsertKey(k KeyRecord) error {
	modelsJSON, err := json.Marshal(k.Models)
	if err != nil {
		return fmt.Errorf("store: UpsertKey marshal models: %w", err)
	}
	revoked := 0
	if k.Revoked {
		revoked = 1
	}
	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO runtime_keys
			(name, key, rate_limit, daily_limit, monthly_limit, models, revoked)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.Name, k.Key, k.RateLimit, k.DailyLimit, k.MonthlyLimit, string(modelsJSON), revoked,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertKey: %w", err)
	}
	return nil
}

func (s *sqliteStore) RevokeKey(name string) error {
	_, err := s.db.Exec(
		`UPDATE runtime_keys SET revoked=1 WHERE name=?`, name,
	)
	if err != nil {
		return fmt.Errorf("store: RevokeKey: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllKeys() ([]KeyRecord, error) {
	rows, err := s.db.Query(
		`SELECT name, key, rate_limit, daily_limit, monthly_limit, models, revoked FROM runtime_keys`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllKeys: %w", err)
	}
	defer rows.Close()

	var keys []KeyRecord
	for rows.Next() {
		var k KeyRecord
		var modelsJSON string
		var revoked int
		if err := rows.Scan(&k.Name, &k.Key, &k.RateLimit, &k.DailyLimit, &k.MonthlyLimit, &modelsJSON, &revoked); err != nil {
			return nil, fmt.Errorf("store: AllKeys scan: %w", err)
		}
		k.Revoked = revoked != 0
		if strings.TrimSpace(modelsJSON) != "" && modelsJSON != "null" {
			if err := json.Unmarshal([]byte(modelsJSON), &k.Models); err != nil {
				return nil, fmt.Errorf("store: AllKeys unmarshal models: %w", err)
			}
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllKeys rows: %w", err)
	}
	return keys, nil
}

// --- Audit log ---

func (s *sqliteStore) AppendAuditLog(e AuditEntry) error {
	cloud := 0
	if e.Cloud {
		cloud = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_log (ts, request_id, key_name, model, node, status, latency_ms, cloud, cloud_model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Time.UTC().Format(time.RFC3339Nano), e.RequestID, e.KeyName, e.Model,
		e.Node, e.Status, e.LatencyMs, cloud, e.CloudModel,
	)
	if err != nil {
		return fmt.Errorf("store: AppendAuditLog: %w", err)
	}
	// Trim to last 10000 entries to prevent unbounded growth.
	_, _ = s.db.Exec(`DELETE FROM audit_log WHERE id NOT IN (SELECT id FROM audit_log ORDER BY id DESC LIMIT 10000)`)
	return nil
}

func (s *sqliteStore) QueryAuditLog(opts AuditQuery) ([]AuditEntry, error) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}

	query := `SELECT ts, request_id, key_name, model, node, status, latency_ms, cloud, cloud_model
	          FROM audit_log WHERE 1=1`
	args := []interface{}{}

	if !opts.Since.IsZero() {
		query += " AND ts > ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	if opts.Model != "" {
		query += " AND model LIKE ?"
		args = append(args, "%"+opts.Model+"%")
	}
	if opts.Key != "" {
		query += " AND key_name = ?"
		args = append(args, opts.Key)
	}
	if opts.Cloud != nil {
		cloud := 0
		if *opts.Cloud {
			cloud = 1
		}
		query += " AND cloud = ?"
		args = append(args, cloud)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, opts.Limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: QueryAuditLog: %w", err)
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var tsStr string
		var cloud int
		if err := rows.Scan(&tsStr, &e.RequestID, &e.KeyName, &e.Model,
			&e.Node, &e.Status, &e.LatencyMs, &cloud, &e.CloudModel); err != nil {
			return nil, fmt.Errorf("store: QueryAuditLog scan: %w", err)
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, tsStr)
		e.Cloud = cloud != 0
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: QueryAuditLog rows: %w", err)
	}
	return entries, nil
}

// --- Admin credentials ---

func (s *sqliteStore) GetAdminCreds() (AdminCreds, error) {
	var c AdminCreds
	err := s.db.QueryRow(
		`SELECT username, password_hash, salt FROM admin_credentials WHERE id=1`,
	).Scan(&c.Username, &c.PasswordHash, &c.Salt)
	if err == sql.ErrNoRows {
		return AdminCreds{}, ErrNoAdminCreds
	}
	if err != nil {
		return AdminCreds{}, fmt.Errorf("store: GetAdminCreds: %w", err)
	}
	return c, nil
}

func (s *sqliteStore) SetAdminCreds(c AdminCreds) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO admin_credentials (id, username, password_hash, salt) VALUES (1, ?, ?, ?)`,
		c.Username, c.PasswordHash, c.Salt,
	)
	if err != nil {
		return fmt.Errorf("store: SetAdminCreds: %w", err)
	}
	return nil
}

// --- Admin sessions ---

func (s *sqliteStore) CreateSession(token string, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO admin_sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		token, time.Now().Unix(), expiresAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: CreateSession: %w", err)
	}
	return nil
}

func (s *sqliteStore) ValidateSession(token string) (bool, error) {
	var expiresAt int64
	err := s.db.QueryRow(
		`SELECT expires_at FROM admin_sessions WHERE token=?`, token,
	).Scan(&expiresAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: ValidateSession: %w", err)
	}
	return time.Now().Unix() < expiresAt, nil
}

func (s *sqliteStore) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM admin_sessions WHERE token=?`, token)
	if err != nil {
		return fmt.Errorf("store: DeleteSession: %w", err)
	}
	return nil
}

func (s *sqliteStore) PruneExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM admin_sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: PruneExpiredSessions: %w", err)
	}
	return nil
}

// --- Users ---

func (s *sqliteStore) CreateUser(u User) (int64, error) {
	mcp := 0
	if u.MustChangePassword {
		mcp = 1
	}
	var approvedAt sql.NullInt64
	if u.ApprovedAt != nil {
		approvedAt = sql.NullInt64{Int64: u.ApprovedAt.Unix(), Valid: true}
	}
	result, err := s.db.Exec(
		`INSERT INTO users
			(username, email, password_hash, salt, role, status, api_key_name,
			 must_change_password, created_at, approved_at, approved_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Username, u.Email, u.PasswordHash, u.Salt, u.Role, u.Status, u.APIKeyName,
		mcp, u.CreatedAt.Unix(), approvedAt, u.ApprovedBy,
	)
	if err != nil {
		return 0, fmt.Errorf("store: CreateUser: %w", err)
	}
	return result.LastInsertId()
}

func (s *sqliteStore) scanUserRow(row *sql.Row) (User, error) {
	var u User
	var mcp int
	var createdAt int64
	var approvedAt, deletedAt sql.NullInt64
	err := row.Scan(
		&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Salt,
		&u.Role, &u.Status, &u.APIKeyName, &mcp, &createdAt,
		&approvedAt, &u.ApprovedBy,
		&deletedAt, &u.DeletedBy,
	)
	if err == sql.ErrNoRows {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: scan user: %w", err)
	}
	u.MustChangePassword = mcp != 0
	u.CreatedAt = time.Unix(createdAt, 0).UTC()
	if approvedAt.Valid {
		t := time.Unix(approvedAt.Int64, 0).UTC()
		u.ApprovedAt = &t
	}
	if deletedAt.Valid {
		t := time.Unix(deletedAt.Int64, 0).UTC()
		u.DeletedAt = &t
	}
	return u, nil
}

const userSelectCols = `id, username, email, password_hash, salt, role, status,
	api_key_name, must_change_password, created_at, approved_at, approved_by,
	deleted_at, deleted_by`

func (s *sqliteStore) GetUserByUsername(username string) (User, error) {
	return s.scanUserRow(s.db.QueryRow(
		`SELECT `+userSelectCols+` FROM users WHERE username=? COLLATE NOCASE`, username,
	))
}

func (s *sqliteStore) GetUserByID(id int64) (User, error) {
	return s.scanUserRow(s.db.QueryRow(
		`SELECT `+userSelectCols+` FROM users WHERE id=?`, id,
	))
}

func (s *sqliteStore) ListUsers() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT ` + userSelectCols + ` FROM users WHERE deleted_at IS NULL ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: ListUsers: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var mcp int
		var createdAt int64
		var approvedAt, deletedAt sql.NullInt64
		if err := rows.Scan(
			&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Salt,
			&u.Role, &u.Status, &u.APIKeyName, &mcp, &createdAt,
			&approvedAt, &u.ApprovedBy,
			&deletedAt, &u.DeletedBy,
		); err != nil {
			return nil, fmt.Errorf("store: ListUsers scan: %w", err)
		}
		u.MustChangePassword = mcp != 0
		u.CreatedAt = time.Unix(createdAt, 0).UTC()
		if approvedAt.Valid {
			t := time.Unix(approvedAt.Int64, 0).UTC()
			u.ApprovedAt = &t
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ListUsers rows: %w", err)
	}
	return users, nil
}

func (s *sqliteStore) UpdateUser(u User) error {
	mcp := 0
	if u.MustChangePassword {
		mcp = 1
	}
	var approvedAt sql.NullInt64
	if u.ApprovedAt != nil {
		approvedAt = sql.NullInt64{Int64: u.ApprovedAt.Unix(), Valid: true}
	}
	_, err := s.db.Exec(
		`UPDATE users SET username=?, email=?, password_hash=?, salt=?, role=?, status=?,
		 api_key_name=?, must_change_password=?, approved_at=?, approved_by=?
		 WHERE id=?`,
		u.Username, u.Email, u.PasswordHash, u.Salt, u.Role, u.Status,
		u.APIKeyName, mcp, approvedAt, u.ApprovedBy, u.ID,
	)
	if err != nil {
		return fmt.Errorf("store: UpdateUser: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: DeleteUser: %w", err)
	}
	return nil
}

func (s *sqliteStore) SoftDeleteUser(id int64, deletedBy string) error {
	_, err := s.db.Exec(
		`UPDATE users SET deleted_at=unixepoch(), deleted_by=?, status='suspended' WHERE id=?`,
		deletedBy, id,
	)
	if err != nil {
		return fmt.Errorf("store: SoftDeleteUser: %w", err)
	}
	return nil
}

func (s *sqliteStore) CountAdminUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND deleted_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: CountAdminUsers: %w", err)
	}
	return n, nil
}

func (s *sqliteStore) PendingUserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE status='pending' AND deleted_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: PendingUserCount: %w", err)
	}
	return n, nil
}

// --- User sessions ---

func (s *sqliteStore) CreateUserSession(us UserSession) error {
	mcp := 0
	if us.MustChangePassword {
		mcp = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO user_sessions
			(token, user_id, role, username, must_change_password, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		us.Token, us.UserID, us.Role, us.Username, mcp, time.Now().Unix(), us.ExpiresAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: CreateUserSession: %w", err)
	}
	return nil
}

func (s *sqliteStore) GetUserSession(token string) (UserSession, bool, error) {
	var us UserSession
	var mcp int
	var expiresAt int64
	err := s.db.QueryRow(
		`SELECT token, user_id, role, username, must_change_password, expires_at
		 FROM user_sessions WHERE token=?`, token,
	).Scan(&us.Token, &us.UserID, &us.Role, &us.Username, &mcp, &expiresAt)
	if err == sql.ErrNoRows {
		return UserSession{}, false, nil
	}
	if err != nil {
		return UserSession{}, false, fmt.Errorf("store: GetUserSession: %w", err)
	}
	if time.Now().Unix() >= expiresAt {
		return UserSession{}, false, nil
	}
	us.MustChangePassword = mcp != 0
	us.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return us, true, nil
}

func (s *sqliteStore) DeleteUserSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM user_sessions WHERE token=?`, token)
	if err != nil {
		return fmt.Errorf("store: DeleteUserSession: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteUserSessionsByUserID(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM user_sessions WHERE user_id=?`, userID)
	if err != nil {
		return fmt.Errorf("store: DeleteUserSessionsByUserID: %w", err)
	}
	return nil
}

func (s *sqliteStore) PruneExpiredUserSessions() error {
	_, err := s.db.Exec(`DELETE FROM user_sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: PruneExpiredUserSessions: %w", err)
	}
	return nil
}

// --- Migration helpers ---

func (s *sqliteStore) HasAdminCredentials() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_credentials`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: HasAdminCredentials: %w", err)
	}
	return n > 0, nil
}

func (s *sqliteStore) GetLegacyAdminCreds() (string, string, string, error) {
	var username, hash, salt string
	err := s.db.QueryRow(
		`SELECT username, password_hash, salt FROM admin_credentials WHERE id=1`,
	).Scan(&username, &hash, &salt)
	if err == sql.ErrNoRows {
		return "", "", "", ErrNoAdminCreds
	}
	if err != nil {
		return "", "", "", fmt.Errorf("store: GetLegacyAdminCreds: %w", err)
	}
	return username, hash, salt, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// --- Routing rules ---

func (s *sqliteStore) UpsertRoutingRule(r RoutingRuleRecord) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO routing_rules (id, condition, target, priority, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Condition, r.Target, r.Priority, enabled, r.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: UpsertRoutingRule: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteRoutingRule(id string) error {
	_, err := s.db.Exec(`DELETE FROM routing_rules WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: DeleteRoutingRule: %w", err)
	}
	return nil
}

func (s *sqliteStore) SetRoutingRuleEnabled(id string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(`UPDATE routing_rules SET enabled=? WHERE id=?`, e, id)
	if err != nil {
		return fmt.Errorf("store: SetRoutingRuleEnabled: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllRoutingRules() ([]RoutingRuleRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, condition, target, priority, enabled, created_at FROM routing_rules ORDER BY priority DESC, created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllRoutingRules: %w", err)
	}
	defer rows.Close()

	var rules []RoutingRuleRecord
	for rows.Next() {
		var r RoutingRuleRecord
		var enabled int
		var createdAt int64
		if err := rows.Scan(&r.ID, &r.Condition, &r.Target, &r.Priority, &enabled, &createdAt); err != nil {
			return nil, fmt.Errorf("store: AllRoutingRules scan: %w", err)
		}
		r.Enabled = enabled != 0
		r.CreatedAt = time.Unix(createdAt, 0).UTC()
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllRoutingRules rows: %w", err)
	}
	return rules, nil
}

// --- Settings key-value store ---

func (s *sqliteStore) GetSetting(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: GetSetting: %w", err)
	}
	return value, nil
}

func (s *sqliteStore) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("store: SetSetting: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("store: AllSettings: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: AllSettings scan: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllSettings rows: %w", err)
	}
	return out, nil
}

// --- Cloud providers ---

func (s *sqliteStore) UpsertCloudProvider(cp CloudProviderRecord) error {
	enabled := 0
	if cp.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO cloud_providers
			(name, provider, base_url, api_key, default_model, cost_per_1k_tokens, enabled, priority)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.Name, cp.Provider, cp.BaseURL, cp.APIKey, cp.DefaultModel,
		cp.CostPer1KTokens, enabled, cp.Priority,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertCloudProvider: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteCloudProvider(name string) error {
	_, err := s.db.Exec(`DELETE FROM cloud_providers WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("store: DeleteCloudProvider: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllCloudProviders() ([]CloudProviderRecord, error) {
	rows, err := s.db.Query(
		`SELECT name, provider, base_url, api_key, default_model, cost_per_1k_tokens, enabled, priority
		 FROM cloud_providers ORDER BY priority DESC, name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllCloudProviders: %w", err)
	}
	defer rows.Close()

	var providers []CloudProviderRecord
	for rows.Next() {
		var cp CloudProviderRecord
		var enabled int
		if err := rows.Scan(
			&cp.Name, &cp.Provider, &cp.BaseURL, &cp.APIKey, &cp.DefaultModel,
			&cp.CostPer1KTokens, &enabled, &cp.Priority,
		); err != nil {
			return nil, fmt.Errorf("store: AllCloudProviders scan: %w", err)
		}
		cp.Enabled = enabled != 0
		providers = append(providers, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllCloudProviders rows: %w", err)
	}
	return providers, nil
}

// --- Warmup configuration ---

func (s *sqliteStore) GetWarmupConfig() (WarmupConfigRecord, error) {
	var w WarmupConfigRecord
	var enabled int
	err := s.db.QueryRow(`SELECT enabled, keep_alive FROM warmup_config WHERE id=1`).Scan(&enabled, &w.KeepAlive)
	if err == sql.ErrNoRows {
		return WarmupConfigRecord{}, ErrNotFound
	}
	if err != nil {
		return WarmupConfigRecord{}, fmt.Errorf("store: GetWarmupConfig: %w", err)
	}
	w.Enabled = enabled != 0
	return w, nil
}

func (s *sqliteStore) SetWarmupConfig(enabled bool, keepAlive string) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO warmup_config (id, enabled, keep_alive) VALUES (1, ?, ?)`,
		e, keepAlive,
	)
	if err != nil {
		return fmt.Errorf("store: SetWarmupConfig: %w", err)
	}
	return nil
}

func (s *sqliteStore) UpsertWarmupModel(model string, nodes []string) error {
	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		return fmt.Errorf("store: UpsertWarmupModel marshal: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO warmup_models (model, nodes_json) VALUES (?, ?)`,
		model, string(nodesJSON),
	)
	if err != nil {
		return fmt.Errorf("store: UpsertWarmupModel: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteWarmupModel(model string) error {
	_, err := s.db.Exec(`DELETE FROM warmup_models WHERE model=?`, model)
	if err != nil {
		return fmt.Errorf("store: DeleteWarmupModel: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllWarmupModels() ([]WarmupModelRecord, error) {
	rows, err := s.db.Query(`SELECT model, nodes_json FROM warmup_models ORDER BY model ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: AllWarmupModels: %w", err)
	}
	defer rows.Close()

	var models []WarmupModelRecord
	for rows.Next() {
		var m WarmupModelRecord
		var nodesJSON string
		if err := rows.Scan(&m.Model, &nodesJSON); err != nil {
			return nil, fmt.Errorf("store: AllWarmupModels scan: %w", err)
		}
		if err := json.Unmarshal([]byte(nodesJSON), &m.Nodes); err != nil {
			m.Nodes = nil
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllWarmupModels rows: %w", err)
	}
	return models, nil
}

// --- Warm state (model residency map) ---

// warmUsedToUnix encodes a last-used time as a Unix second count, mapping the
// zero time (never used) to 0 so it round-trips through the INTEGER column.
func warmUsedToUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// warmUnixToUsed is the inverse of warmUsedToUnix.
func warmUnixToUsed(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

func (s *sqliteStore) RecordWarmLoad(w WarmStateRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO warm_state (model, node, last_used, vram_bytes, load_count)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT(model, node) DO UPDATE SET
				last_used  = excluded.last_used,
				vram_bytes = excluded.vram_bytes,
				load_count = warm_state.load_count + 1`,
		w.Model, w.Node, warmUsedToUnix(w.LastUsed), w.VRAMBytes,
	)
	if err != nil {
		return fmt.Errorf("store: RecordWarmLoad: %w", err)
	}
	return nil
}

func (s *sqliteStore) SnapshotWarmState(w WarmStateRecord) error {
	// Refresh residency/last-used/vram without bumping load_count — this is the
	// periodic snapshot flush, not a load event. Inserts a fresh row (load_count
	// 0) only if the pair is not already present.
	_, err := s.db.Exec(
		`INSERT INTO warm_state (model, node, last_used, vram_bytes, load_count)
			VALUES (?, ?, ?, ?, 0)
			ON CONFLICT(model, node) DO UPDATE SET
				last_used  = excluded.last_used,
				vram_bytes = excluded.vram_bytes`,
		w.Model, w.Node, warmUsedToUnix(w.LastUsed), w.VRAMBytes,
	)
	if err != nil {
		return fmt.Errorf("store: SnapshotWarmState: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteWarmState(model, node string) error {
	_, err := s.db.Exec(`DELETE FROM warm_state WHERE model=? AND node=?`, model, node)
	if err != nil {
		return fmt.Errorf("store: DeleteWarmState: %w", err)
	}
	return nil
}

func (s *sqliteStore) DeleteWarmStateByNode(node string) error {
	_, err := s.db.Exec(`DELETE FROM warm_state WHERE node=?`, node)
	if err != nil {
		return fmt.Errorf("store: DeleteWarmStateByNode: %w", err)
	}
	return nil
}

func (s *sqliteStore) AllWarmState() ([]WarmStateRecord, error) {
	rows, err := s.db.Query(
		`SELECT model, node, last_used, vram_bytes, load_count FROM warm_state`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: AllWarmState: %w", err)
	}
	defer rows.Close()

	var out []WarmStateRecord
	for rows.Next() {
		var w WarmStateRecord
		var lastUsed int64
		if err := rows.Scan(&w.Model, &w.Node, &lastUsed, &w.VRAMBytes, &w.LoadCount); err != nil {
			return nil, fmt.Errorf("store: AllWarmState scan: %w", err)
		}
		w.LastUsed = warmUnixToUsed(lastUsed)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: AllWarmState rows: %w", err)
	}
	return out, nil
}

// ReconcileNodeWarmState makes the persisted residency for a node exactly match
// the live /api/ps truth by deleting every warm_state row for the node whose
// model is NOT in residentModels. When residentModels is empty every row for
// that node is deleted. A single SQL statement is used; no row-by-row loop.
func (s *sqliteStore) ReconcileNodeWarmState(node string, residentModels []string) error {
	if len(residentModels) == 0 {
		// Fast path: node is fully cold — clear it entirely.
		_, err := s.db.Exec(`DELETE FROM warm_state WHERE node = ?`, node)
		if err != nil {
			return fmt.Errorf("store: ReconcileNodeWarmState clear %s: %w", node, err)
		}
		return nil
	}

	// Build a single DELETE … WHERE node = ? AND model NOT IN (?, ?, …).
	// We build the placeholder list once; SQLite handles the IN clause natively.
	placeholders := make([]string, len(residentModels))
	args := make([]any, 0, 1+len(residentModels))
	args = append(args, node)
	for i, m := range residentModels {
		placeholders[i] = "?"
		args = append(args, m)
	}
	query := `DELETE FROM warm_state WHERE node = ? AND model NOT IN (` +
		strings.Join(placeholders, ",") + `)`
	if _, err := s.db.Exec(query, args...); err != nil {
		return fmt.Errorf("store: ReconcileNodeWarmState %s: %w", node, err)
	}
	return nil
}
