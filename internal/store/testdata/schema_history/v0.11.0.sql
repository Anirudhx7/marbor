@@STMT@@
CREATE TABLE IF NOT EXISTS request_log (
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
			name     TEXT PRIMARY KEY,
			draining INTEGER
		)
@@STMT@@
CREATE TABLE IF NOT EXISTS runtime_keys (
			name          TEXT PRIMARY KEY,
			key           TEXT,
			rate_limit    INTEGER,
			daily_limit   INTEGER,
			monthly_limit INTEGER,
			models        TEXT,
			revoked       INTEGER
		)
