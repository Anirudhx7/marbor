-- Clear existing data
DELETE FROM runtime_nodes;
DELETE FROM runtime_keys;
DELETE FROM users;
DELETE FROM user_sessions;
DELETE FROM settings;
DELETE FROM request_log;

-- Insert admin user (username: admin, password hash of "admin")
-- Password hash: $2a$10$P61YB8dFDRNJZumNNZrhgeyziqeHyIJ68UsaxkpDwMMV//AMHvfbK
-- (regenerated 2026-08-07: the previous hash did not actually verify against
-- "admin" despite the comment - login always failed against it, masked until
-- now by smoke.sh's CLI check using the pre-seeded session token below
-- instead of a real login)
INSERT INTO users (id, username, email, password_hash, salt, role, status, api_key_name, must_change_password, created_at, approved_at, approved_by, deleted_at, deleted_by)
VALUES (1, 'admin', 'admin@local', '$2a$10$P61YB8dFDRNJZumNNZrhgeyziqeHyIJ68UsaxkpDwMMV//AMHvfbK', '', 'admin', 'active', '', 0, 1783087285, 1783087285, 'system', NULL, '');

-- Insert persistent session for smoke test / demo access. The stored token
-- is SHA-256('demo-admin-token') hex-encoded, not the plaintext value -
-- sqlite.go's GetUserSession/CreateUserSession hash every session token at
-- rest, so the client-facing token used by smoke.sh (and anyone
-- hitting the demo stack) stays 'demo-admin-token' in the Bearer header,
-- while this table stores only its hash.
INSERT INTO user_sessions (token, user_id, role, username, must_change_password, created_at, expires_at)
VALUES ('91c16f0d6cc1bec3c3603972182a07c66ff4fa71618a975e963d6dbe42b6dd37', 1, 'admin', 'admin', 0, 1783087285, 2000000000);

-- Insert runtime keys
INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked)
VALUES ('demo-key', 'demo-api-key', 1000, 0, 0, '', 0);
INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked)
VALUES ('soak-test', 'demo-soak-key', 0, 0, 0, '', 0);

-- Insert runtime nodes
INSERT INTO runtime_nodes (name, url, runtime, vram_total_mb)
VALUES ('node-a', 'http://ollama-node-a:11434', 'ollama', 24576),
       ('node-b', 'http://ollama-node-b:11434', 'ollama', 24576),
       ('vllm-node', 'http://vllm-node:11434', 'vllm', 24576),
       ('tgi-node', 'http://tgi-node:11434', 'tgi', 24576),
       ('llamacpp-node', 'http://llamacpp-node:11434', 'llamacpp', 24576),
       ('mlx-node', 'http://mlx-node:11434', 'mlx', 24576);

-- Insert settings
INSERT INTO settings (key, value)
VALUES ('metrics_enabled', 'true'),
       ('routing_strategy', 'warm-first');

-- Sample request log rows showing the prompt-cache split: values where the
-- runtime reports it, one real zero, and NULL (unknown) where it does not.
-- They age out of the 1000-row log once live traffic arrives.
INSERT INTO request_log (id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts, routing_reason, routing_detail, prefill_ms, cached_tokens, input_tokens)
VALUES ('req-demo-cache-01', 'demo-key', 'qwen2.5:7b', 'vllm-node', 200, 240, 2020, 0, 'vllm-node', 0, 1783087100, 'score_based', '', 0, 1820, 2000),
       ('req-demo-cache-02', 'demo-key', 'qwen2.5:7b', 'vllm-node', 200, 310, 1455, 0, 'vllm-node', 0, 1783087120, 'score_based', '', 0, 0, 1450),
       ('req-demo-cache-03', 'demo-key', 'qwen2.5:7b', 'vllm-node', 200, 190, 1010, 0, 'vllm-node', 0, 1783087140, 'session_affinity', '', 0, 640, 800),
       ('req-demo-cache-04', 'demo-key', 'llama3.2:3b', 'llamacpp-node', 200, 150, 128, 0, 'llamacpp-node', 0, 1783087160, 'score_based', '', 0, 100, 120),
       ('req-demo-cache-05', 'demo-key', 'llama3.2:3b', 'node-a', 200, 120, 316, 0, 'node-a', 0, 1783087180, 'pinned_warm', '', 9, NULL, NULL),
       ('req-demo-cache-06', 'demo-key', 'mistral:7b', 'node-b', 200, 180, 412, 0, 'node-b', 0, 1783087200, 'score_based', '', 14, NULL, NULL),
       ('req-demo-cache-07', 'demo-key', 'mistral:7b', 'node-b', 200, 175, 380, 0, 'node-b', 0, 1783087220, 'score_based', '', 12, NULL, NULL);
