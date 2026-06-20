# Changelog

All notable changes to this project will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

## [0.9.1] - 2026-06-20

### Changed
- Dashboard sidebar is now a slide-in drawer on mobile with a hamburger toggle and backdrop overlay. Desktop behavior unchanged.
- Main content area has correct top padding on mobile to clear the hamburger button.
- Fixed non-responsive `grid-cols-2/3` layouts in API Keys edit modal, GPU Nodes stat panel, and Routing rule form - all now collapse to single column on small screens.

## [0.9.0] - 2026-06-19

### Added
- **Model warmup** - proactive `keep_alive` pings on a configurable schedule keep loaded models hot in VRAM. Eliminates cold starts on first request after idle.
- **SIGHUP full config reload** - `kill -HUP <pid>` diffs the new config against the running state: adds new nodes/cloud providers/auth keys, removes deleted ones, leaves unchanged ones running. Zero-downtime reconfiguration without a process restart.
- **Structured JSON logging** via `log/slog`. `--log-format json` outputs one JSON object per line. Default remains plain text. Legacy `log.Printf` calls redirect through the same handler.
- **Node drain** - `POST /admin/nodes/{name}/drain` marks a node so the router skips it for new requests while in-flight requests complete. `DELETE` to undrain. Enables zero-downtime GPU maintenance. Dashboard shows an amber DRAINING badge.
- **Per-node request counter** - `requests_total` tracked atomically per node, exposed in `GET /admin/nodes`.
- **X-Request-ID forwarded upstream** - the proxy now sets `X-Request-ID` on requests forwarded to Ollama nodes for end-to-end log correlation.
- **Retry-After + quota reset headers** on 429 responses - clients see when to retry.
- **`POST /admin/v1/config/reload`** - HTTP-triggered config reload for container environments that cannot send SIGHUP.
- **`GET /admin/v1/config`** - returns the current live config (secrets masked). Settings page shows a Reload button.
- **`PATCH /admin/nodes/{name}`** - runtime metadata overrides: `vram_total_mb`, `gpu_model`.
- **`PATCH /admin/keys/{name}`** (`PatchKey`) - update rate limit, daily/monthly quotas, and model allow-list at runtime without rotating the key. Counters preserved.
- Queue depth metric card on the dashboard.

## [0.8.2] - 2026-06-19

### Added
- **Request queue** - instead of returning 503 when all nodes are at connection capacity, requests queue and retry as slots free. Configurable `routing.queue_max_depth` and `routing.queue_timeout_ms`.

## [0.8.1] - 2026-06-18

### Fixed
- Quota counters no longer drift when multiple requests race the monthly reset window.
- `nvidia-smi` is polled at the configured `routing.poll_interval_ms` interval, not on every health check.
- Docker auto-discovery now works on Windows (path normalization for Docker Desktop socket).

## [0.8.0] - 2026-06-18

### Added
- **VRAM-aware cold routing** - when no node has a model warm, the router places the request on the node with the most free VRAM rather than using least-connections. Nodes with unknown capacity or overcommitted VRAM are skipped; falls back to least-connections when all nodes are at capacity.

## [0.7.0] - 2026-06-18

### Added
- **Active/active HA** - each instance polls a configurable peer list (`ha.peers`) via `/health`. `GET /admin/ha/peers` returns cluster status. Run two instances behind any TCP load balancer; the proxy SPOF is eliminated.

## [0.6.0] - 2026-06-17

### Added
- **KV-cache / context affinity** - sticky session routing via `X-Session-ID` request header. Same conversation ID routes to the same node, keeping the KV cache warm for multi-turn inference. TTL-based eviction; falls back gracefully on node failure.
- **Warm-vs-cold benchmark** - `make bench` runs a reproducible first-token latency comparison (warm model in VRAM vs cold load). Numbers are measured on real hardware. See `bench/`.
- Security regression tests for five invariants: constant-time admin token compare, auth fails closed when enabled, cloud off by default when unconfigured, request body cap enforced, upstream host comes only from config.

## [0.5.0] - 2026-06-17

### Security
- Config file is now written `0600` (owner-only) and an existing file is chmod-ed back to `0600` on every save. It previously used `0644` and re-widened on every dashboard "Save Settings", exposing all API keys, the admin token, and cloud-provider keys in plaintext to any local user.
- The admin server no longer falls back to the literal token `admin` when no `admin_token` is set. It generates a `crypto/rand` token and logs it once at startup, closing a LAN-takeover path (guessable token + all-interfaces admin bind).
- `GET /admin/keys` no longer returns proxy keys in plaintext. It returns a masked prefix; the full key is shown only once at creation. The dashboard's reveal/copy-from-list controls were removed accordingly.
- Cloud-fallback now uses a dedicated transport with `ResponseHeaderTimeout` (from `routing.upstream_timeout_ms`) instead of `http.DefaultTransport`, so a hung provider can no longer leak goroutines/connections. No overall client timeout is set, so streaming is preserved.

### Added
- `routing.allow_management_endpoints` (default `false`).

### Changed
- **Behavior change:** the proxy now rejects Ollama model-management endpoints (`/api/delete`, `/api/pull`, `/api/push`, `/api/create`, `/api/copy`, `/api/blobs`) with `403` by default. Any authenticated key could previously delete or pull models on a backend node. Set `routing.allow_management_endpoints: true` to restore the old behavior for single-tenant deployments. Inference and read-only inventory paths (`/api/generate`, `/api/chat`, `/api/tags`, `/v1/*`, ...) are unaffected.

## [0.4.0] - 2026-06-16

### Added
- Cluster-wide remote-node VRAM telemetry. Each node reports a VRAM source (`nvidia`, `declared`, `api`, or `none`) so the dashboard is honest about where the number came from: `nvidia-smi` is read only for local nodes, remote nodes fall back to a declared `vram_total_mb` (new optional per-node config field) or the VRAM derived from Ollama's `/api/ps` (`size_vram`), and show `none` when nothing is known.

### Fixed
- API key expiry is now enforced. A key past `expires_at` (date `2006-01-02`, valid through end of day, or RFC3339) is rejected before rate-limiting; unparseable values are treated as non-expiring.
- Allow-list rejections (`403`) no longer consume a key's rate-limit/quota budget - the token is refunded, so a blocked model never exhausts a key into a `429`.
- Cloud overflow with `stream: false` now returns a single Ollama JSON object (not an NDJSON stream), matching native Ollama semantics.
- Several concurrency and correctness fixes: double connection-decrement on pre-byte proxy errors, two data races (`handleSummary`/`handleKeys` reading node and key state without the lock), a quota check-then-increment TOCTOU, and SSE response truncation on the cloud-translation path.
- Prometheus `model` label is now bounded (cap 256 distinct values, overflow folded to `other`, empty to `unknown`) to prevent client-controlled label cardinality blowup.

## [0.3.0] - 2026-06-14

### Added
- Per-key usage and hard quotas (`daily_limit` / `monthly_limit`) that persist across restarts via an atomic JSON state file (`auth.state_path`, default `usage-state.json`, `-` to disable). A restart no longer resets quotas or usage.
- Per-key model allow-lists enforced at the proxy: requests for a model outside a key's list return `403`.
- `GET /v1/models` returns an aggregated OpenAI-schema list of models across all healthy nodes.
- Cloud-overflow response translation: OpenAI/Anthropic responses are translated back to Ollama's NDJSON shape on the native (`/api/*`) path, including SSE-to-NDJSON streaming.
- Upstream retry/failover: a node that fails before sending any bytes is retried on an alternate node (`routing.max_retries`), then cloud, then 502 - never violates streaming.
- `routing.upstream_timeout_ms`: bounds the wait for upstream response headers (header phase only, not the stream body).
- CLI flags: `--version`, `--config <path>`, `--validate` (validate config and exit), and `--help`.
- 4 new Prometheus metrics (11 total): `ollamamesh_retries_total`, `ollamamesh_cloud_fallbacks_total`, `ollamamesh_quota_rejections_total`, `ollamamesh_panics_total`.
- Structured JSON access log on stdout, one line per request (`proxy.access_log`, default on). Logs key name (never the value), model, node, status, latency, request id.
- Configurable admin listener: `admin.bind_address` (default `:8080`; set `127.0.0.1:8080` to keep the dashboard off the network) and `admin.cors_origin`.
- `make demo`: spins up mock Ollama servers in-process, sends real traffic, shows a populated dashboard in <60s with no Ollama install required.
- VRAM fit indicator: GPU Nodes page shows green/yellow/red badges for each downloaded model per node based on available VRAM.
- Tokens and tok/s columns in live request log.
- `docs/PRODUCTION.md`, `docs/INTEGRATIONS.md`, and an honest "How It Compares" table in the README.

### Changed
- Zero-config first run promoted to top-level Quick Start path in README.
- Savings reference rate is now configurable via `savings.reference_cost_per_1k` (see v0.2.1).

### Security
- Admin token comparison is now constant-time (`crypto/subtle.ConstantTimeCompare`) to remove a timing side channel.
- Request bodies are capped at 32 MiB (`413` over the limit) to bound a memory-exhaustion DoS vector.
- Admin API no longer sends a wildcard `Access-Control-Allow-Origin: *`. CORS is off by default (same-origin only) and opt-in via `admin.cors_origin`, closing a CSRF/exfil surface on the mutating admin API.
- `ReadHeaderTimeout` set on the proxy, admin, and metrics servers (metrics server previously had no timeouts) to close a Slowloris DoS vector.
- Upstream/cloud error responses no longer leak the raw error (hostnames, ports, dial/TLS details) to the client; they return a generic `502 upstream unavailable` and log the detail server-side.
- Panic-recovery middleware returns a clean `500` and increments `ollamamesh_panics_total` instead of dropping the connection. It re-raises `http.ErrAbortHandler` so streaming aborts are unaffected (R2).
- Usage-state persistence now `fsync`s the temp file and parent directory before the atomic rename, closing a power-loss window that could reset quota counters.
- Config validation rejects non-`http(s)` node and cloud `base_url` values at boot instead of failing per-request later.

### Performance
- Hourly analytics buckets are pruned to a 48h window, bounding memory growth on long-lived processes.

### CI
- CI now runs `go vet`, a `gofmt` gate, `go test -race`, and `govulncheck`.

---

## [0.2.1] - 2026-06-11

### Added
- Zero-config first run: `./ollama-mesh` auto-detects localhost:11434, generates crypto/rand API keys, starts on :11435, prints one curl example
- `savings.reference_cost_per_1k` config field: controls the $/1K token rate used for saved_usd calculation (default $0.002)
- `docs/SAVINGS-MATH.md`: formula, null semantics, restart reset, and known limitations documented
- `CloudModel` field in audit log entries: records cloud model used when default_model rewrites the request

### Fixed
- Savings math uses real parsed token counts (eval_count + prompt_eval_count for Ollama, usage.total_tokens for OpenAI); no more hardcoded 500-token estimate
- saved_usd and cloud_spent_usd return JSON null (shows "—" in UI) when requests exist but no token data was parseable
- Mid-stream abort (upstream node death) now records status=aborted in metrics, admin log, and audit instead of silently vanishing
- Cloud model rewriting visible in request log as "original -> cloud_model" for observability

### Tests
- Streaming integration tests: unbuffered delivery (R2 verified), token tracking from NDJSON tail, mid-stream node death records aborted, SSE passthrough
- Admin savings tests: custom reference rate flows to handleSavings and hourly analytics buckets
- Config tests: default rate applied via Validate(), custom rate loaded from YAML
- Proxy tests: cloud model mapping visible in live request log and audit entry

## [0.2.0] - 2026-05-23

### Added
- Analytics dashboard: 24-hour area chart (local vs cloud), cost savings stats, per-model breakdown table
- Model catalog: cross-node VRAM view with warm status badges and node chips, searchable
- Request log page: live feed with 3-second polling, filter by key/model/status, status indicators
- Rate limit response headers: X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset on every response
- Webhook notifications: node_down and node_up events with HMAC-SHA256 request signatures
- Audit logging: append-only JSON-lines file with crypto/rand request IDs
- Docker auto-discovery: scans containers with OLLAMA_HOST env or ollama/ollama image, deduplicates by URL
- Grafana dashboard: 13-panel JSON at grafana/ollama-mesh.json, one-click Prometheus import
- Admin API versioning: all endpoints available at both /admin/ and /admin/v1/
- Cloud provider config: OpenAI and Anthropic fallback routing with per-request cost tracking
- Cloud savings widget on dashboard: shows estimated savings vs pure cloud this month
- GET /health endpoint: unauthenticated, returns 200 when proxy is ready
- X-Request-ID header on all proxy responses

### Fixed
- Auth token validation uses exact match (was substring - security vulnerability)
- Port derivation uses URL parsing (was hardcoded array index arithmetic)
- VRAM calculation uses real Ollama /api/ps data (was fake arithmetic)
- Random CPU/temperature metrics replaced with real nvidia-smi data
- Mutex protection added to auth rate limit maps (was a race condition)
- API key creation timestamp shows real value (was hardcoded date)
- UI auth prompt asks for token (was silently falling back to "admin")

## [0.1.0] - 2026-05-16

### Added
- Initial release
- GPU-aware warm-first routing across Ollama nodes
- Bearer token authentication with per-key rate limiting
- Admin dashboard (React + TypeScript, embedded in binary)
- Prometheus metrics on :9090
- Docker Compose example
- Single Go binary, zero runtime dependencies
