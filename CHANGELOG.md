# Changelog

All notable changes to this project will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added
- `make demo`: spins up mock Ollama servers in-process, sends real traffic, shows a populated dashboard in <60s with no Ollama install required
- VRAM fit indicator: GPU Nodes page shows green/yellow/red badges for each downloaded model per node based on available VRAM
- Tokens and tok/s columns in live request log

### Changed
- Zero-config first run promoted to top-level Quick Start path in README
- Savings reference rate is now configurable via `savings.reference_cost_per_1k` (see v0.2.1)

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
