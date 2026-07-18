-- Clear existing data
DELETE FROM runtime_nodes;
DELETE FROM runtime_keys;
DELETE FROM users;
DELETE FROM user_sessions;
DELETE FROM settings;

-- Insert admin user (username: admin, password hash of "admin")
-- Password hash: $2a$10$jBIxiUqWwZh0GorGJbaLaeo2UYxW5rbN6NzwCVRKYERDAh3ORFBFK
INSERT INTO users (id, username, email, password_hash, salt, role, status, api_key_name, must_change_password, created_at, approved_at, approved_by, deleted_at, deleted_by)
VALUES (1, 'admin', 'admin@local', '$2a$10$jBIxiUqWwZh0GorGJbaLaeo2UYxW5rbN6NzwCVRKYERDAh3ORFBFK', '', 'admin', 'active', '', 0, 1783087285, 1783087285, 'system', NULL, '');

-- Insert persistent session for smoke test / demo access
INSERT INTO user_sessions (token, user_id, role, username, must_change_password, created_at, expires_at)
VALUES ('demo-admin-token', 1, 'admin', 'admin', 0, 1783087285, 2000000000);

-- Insert runtime keys
INSERT INTO runtime_keys (name, key, rate_limit, daily_limit, monthly_limit, models, revoked)
VALUES ('demo-key', 'demo-api-key', 1000, 0, 0, '', 0);

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
