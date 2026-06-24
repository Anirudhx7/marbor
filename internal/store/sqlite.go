package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Serialize all writers through a single connection.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: WAL pragma: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: foreign_keys pragma: %w", err)
	}

	s := &sqliteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
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
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate stmt: %w\nSQL: %s", err, stmt)
		}
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

func (s *sqliteStore) UpsertHourlyBucket(b HourlyBucket) error {
	hourUnix := b.Hour.Truncate(time.Hour).Unix()
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO hourly_buckets
			(hour, requests, tokens, cloud_requests, local_requests, cost_usd)
			VALUES (?, ?, ?, ?, ?, ?)`,
		hourUnix, b.Requests, b.Tokens, b.CloudRequests, b.LocalRequests, b.CostUSD,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertHourlyBucket: %w", err)
	}
	return nil
}

func (s *sqliteStore) HourlyBuckets(since time.Time) ([]HourlyBucket, error) {
	rows, err := s.db.Query(
		`SELECT hour, requests, tokens, cloud_requests, local_requests, cost_usd
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
		if err := rows.Scan(&hourUnix, &b.Requests, &b.Tokens, &b.CloudRequests, &b.LocalRequests, &b.CostUSD); err != nil {
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

func (s *sqliteStore) UpsertModelStat(ms ModelStat) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO model_stats (model, requests, tokens, cost_usd)
		 VALUES (?, ?, ?, ?)`,
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

func (s *sqliteStore) Close() error {
	return s.db.Close()
}
