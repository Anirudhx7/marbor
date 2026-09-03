@@STMT@@
CREATE TABLE IF NOT EXISTS request_log (
			id             TEXT PRIMARY KEY,
			key_name       TEXT,
			model          TEXT,
			node_name      TEXT,
			status_code    INTEGER,
			latency_ms     INTEGER,
			tokens_used    INTEGER,
			cost_usd       REAL,
			routed_to      TEXT,
			is_cloud       INTEGER,
			ts             INTEGER,
			routing_reason TEXT,
			routing_detail TEXT
		)
@@STMT@@
CREATE INDEX IF NOT EXISTS request_log_ts ON request_log(ts DESC)
@@STMT@@
CREATE TABLE IF NOT EXISTS hourly_buckets (
			hour           INTEGER PRIMARY KEY,
			requests       INTEGER,
			tokens         INTEGER,
			cloud_requests INTEGER,
			local_requests INTEGER,
			cost_usd       REAL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS model_stats (
			model    TEXT PRIMARY KEY,
			requests INTEGER,
			tokens   INTEGER,
			cost_usd REAL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS counters (
			id              INTEGER PRIMARY KEY CHECK(id=1),
			local_requests  INTEGER,
			cloud_requests  INTEGER,
			total_tokens    INTEGER,
			cloud_spent_usd REAL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS key_counters (
			name         TEXT PRIMARY KEY,
			today        INTEGER,
			month        INTEGER,
			tokens_today INTEGER,
			tokens_month INTEGER,
			last_reset   INTEGER
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS runtime_nodes (
			name         TEXT PRIMARY KEY,
			url          TEXT,
			runtime      TEXT,
			vram_total_mb INTEGER
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS node_overrides (
			name         TEXT PRIMARY KEY,
			vram_total_mb INTEGER,
			gpu_model    TEXT
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS node_drain (
			name           TEXT PRIMARY KEY,
			draining       INTEGER,
			drained_reason TEXT NOT NULL DEFAULT ''
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS node_agent (
			name    TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			port    INTEGER NOT NULL DEFAULT 0,
			token   TEXT NOT NULL DEFAULT '',
			scope   TEXT NOT NULL DEFAULT 'admin'
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS node_control (
			name                  TEXT PRIMARY KEY,
			driver                TEXT NOT NULL DEFAULT '',
			identifier            TEXT NOT NULL DEFAULT '',
			configured            INTEGER NOT NULL DEFAULT 0,
			discovered_driver     TEXT NOT NULL DEFAULT '',
			discovered_identifier TEXT NOT NULL DEFAULT '',
			discovered_evidence   TEXT NOT NULL DEFAULT '[]',
			start_command         TEXT NOT NULL DEFAULT ''
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS predictive_history (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			from_model TEXT NOT NULL,
			to_model   TEXT NOT NULL,
			ts         TEXT NOT NULL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS runtime_keys (
			name          TEXT PRIMARY KEY,
			key           TEXT,
			rate_limit    INTEGER,
			daily_limit   INTEGER,
			monthly_limit INTEGER,
			daily_usd_cap REAL NOT NULL DEFAULT 0,
			monthly_usd_cap REAL NOT NULL DEFAULT 0,
			models        TEXT,
			revoked       INTEGER,
			expires_at    TEXT NOT NULL DEFAULT ''
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS audit_log (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			ts             TEXT NOT NULL,
			request_id     TEXT NOT NULL,
			key_name       TEXT NOT NULL,
			model          TEXT NOT NULL,
			node           TEXT NOT NULL,
			status         TEXT NOT NULL,
			latency_ms     INTEGER NOT NULL,
			cloud          INTEGER NOT NULL DEFAULT 0,
			cloud_model    TEXT NOT NULL DEFAULT '',
			routing_reason TEXT NOT NULL DEFAULT ''
		)
@@STMT@@
CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(ts DESC)
@@STMT@@
CREATE INDEX IF NOT EXISTS idx_audit_log_filters ON audit_log (key_name, model, node, ts)
@@STMT@@
CREATE TABLE IF NOT EXISTS admin_credentials (
			id            INTEGER PRIMARY KEY CHECK(id=1),
			username      TEXT NOT NULL DEFAULT 'admin',
			password_hash TEXT NOT NULL,
			salt          TEXT NOT NULL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS admin_sessions (
			token      TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS users (
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
			skip_password_count  INTEGER NOT NULL DEFAULT 0,
			created_at           INTEGER NOT NULL,
			approved_at          INTEGER,
			approved_by          TEXT NOT NULL DEFAULT ''
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS user_sessions (
			token                TEXT PRIMARY KEY,
			user_id              INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role                 TEXT NOT NULL,
			username             TEXT NOT NULL,
			must_change_password INTEGER NOT NULL DEFAULT 0,
			created_at           INTEGER NOT NULL,
			expires_at           INTEGER NOT NULL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS favorites (
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			model_id   TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (user_id, model_id)
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS system_audit_log (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			ts        TEXT NOT NULL,
			username  TEXT NOT NULL,
			action    TEXT NOT NULL,
			target    TEXT NOT NULL,
			details   TEXT NOT NULL,
			source_ip TEXT NOT NULL
		)
@@STMT@@
CREATE INDEX IF NOT EXISTS idx_system_audit_log_ts ON system_audit_log(ts DESC)
@@STMT@@
CREATE TABLE IF NOT EXISTS cloud_providers (
			name               TEXT PRIMARY KEY,
			provider           TEXT NOT NULL,
			base_url           TEXT NOT NULL,
			api_key            TEXT NOT NULL DEFAULT '',
			default_model      TEXT NOT NULL DEFAULT '',
			cost_per_1k_tokens REAL NOT NULL DEFAULT 0.0,
			enabled            INTEGER NOT NULL DEFAULT 1,
			priority           INTEGER NOT NULL DEFAULT 0
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS routing_rules (
			id         TEXT PRIMARY KEY,
			condition  TEXT NOT NULL,
			target     TEXT NOT NULL,
			priority   INTEGER NOT NULL DEFAULT 0,
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS warmup_config (
			id         INTEGER PRIMARY KEY CHECK(id=1),
			enabled    INTEGER NOT NULL DEFAULT 0,
			keep_alive TEXT NOT NULL DEFAULT '10m'
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS warmup_models (
			model      TEXT PRIMARY KEY,
			nodes_json TEXT NOT NULL DEFAULT '[]'
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS warm_state (
			model      TEXT NOT NULL,
			node       TEXT NOT NULL,
			last_used  INTEGER NOT NULL DEFAULT 0,
			vram_bytes INTEGER NOT NULL DEFAULT 0,
			load_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (model, node)
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS session_affinity (
			session_id TEXT PRIMARY KEY,
			node_url   TEXT NOT NULL,
			last_seen  INTEGER NOT NULL DEFAULT 0
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS model_configs (
			model       TEXT NOT NULL,
			node        TEXT NOT NULL,
			config_json TEXT NOT NULL,
			PRIMARY KEY (model, node)
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS spill_counters (
			key_name  TEXT NOT NULL,
			served_by TEXT NOT NULL,
			requests  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (key_name, served_by)
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS benchmark_runs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			node         TEXT NOT NULL,
			model        TEXT NOT NULL,
			n            INTEGER NOT NULL,
			cold_p50_ms  REAL NOT NULL,
			cold_min_ms  REAL NOT NULL,
			cold_max_ms  REAL NOT NULL,
			warm_p50_ms  REAL NOT NULL,
			warm_min_ms  REAL NOT NULL,
			warm_max_ms  REAL NOT NULL,
			speedup_x    REAL NOT NULL,
			created_at   INTEGER NOT NULL
		)
@@STMT@@
ALTER TABLE users ADD COLUMN deleted_at INTEGER
@@STMT@@
ALTER TABLE users ADD COLUMN deleted_by TEXT NOT NULL DEFAULT ''
@@STMT@@
ALTER TABLE users ADD COLUMN skip_password_count INTEGER NOT NULL DEFAULT 0
@@STMT@@
ALTER TABLE hourly_buckets ADD COLUMN gen_duration_ms INTEGER NOT NULL DEFAULT 0
@@STMT@@
ALTER TABLE runtime_keys ADD COLUMN daily_usd_cap REAL NOT NULL DEFAULT 0
@@STMT@@
ALTER TABLE runtime_keys ADD COLUMN monthly_usd_cap REAL NOT NULL DEFAULT 0
@@STMT@@
ALTER TABLE runtime_keys ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''
@@STMT@@
ALTER TABLE runtime_keys ADD COLUMN local_only INTEGER NOT NULL DEFAULT 0
@@STMT@@
ALTER TABLE runtime_keys ADD COLUMN allow_local_degradation INTEGER NOT NULL DEFAULT 0
@@STMT@@
ALTER TABLE node_drain ADD COLUMN drained_reason TEXT NOT NULL DEFAULT ''
@@STMT@@
ALTER TABLE node_overrides ADD COLUMN runtime TEXT
@@STMT@@
ALTER TABLE node_overrides ADD COLUMN gpu_indices TEXT NOT NULL DEFAULT ''
@@STMT@@
ALTER TABLE node_overrides ADD COLUMN max_in_flight INTEGER
@@STMT@@
ALTER TABLE node_overrides ADD COLUMN tls_fingerprint TEXT
@@STMT@@
ALTER TABLE node_control ADD COLUMN start_command TEXT NOT NULL DEFAULT ''
@@STMT@@
ALTER TABLE runtime_nodes ADD COLUMN host TEXT
@@STMT@@
ALTER TABLE node_agent ADD COLUMN scope TEXT NOT NULL DEFAULT 'admin'
@@STMT@@
ALTER TABLE request_log ADD COLUMN routing_reason TEXT
@@STMT@@
ALTER TABLE request_log ADD COLUMN routing_detail TEXT
@@STMT@@
ALTER TABLE audit_log ADD COLUMN routing_reason TEXT NOT NULL DEFAULT ''
