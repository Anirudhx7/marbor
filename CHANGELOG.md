# Changelog

All notable changes to this project will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

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
