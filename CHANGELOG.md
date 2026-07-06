# Changelog

All notable changes to this project will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added
- **`uninstall.sh`**: removes the binary, the systemd service (if installed), and a background (nohup) instance; prompts before deleting `config.yaml`/`mesh.db` (kept by default, including in non-interactive/piped runs — `KEEP_DB=0`/`KEEP_CONFIG=0` to remove without prompting).
- **`install.sh` post-install health checks**: after starting (either mode), validates `config.yaml` (`-validate`), confirms the proxy/admin/metrics ports are actually responding (not just that the process exists), and reports reachability of configured backend nodes.
- **`install.sh` upgrade reporting**: prints old → new version on reinstall/upgrade via the binary's own `-version` flag; warns when the binary on disk was upgraded but the running background process hasn't been restarted yet.
- **`install.sh` idempotent background start**: re-running the installer while a `nohup`-started instance is already running no longer starts a competing second process — it detects the existing one (via a new `ollama-mesh.pid`) and re-verifies its health instead. A stale pidfile (process no longer running, e.g. after a crash) is cleaned up and a fresh instance starts normally.

### Changed
- **`SERVICE=1` is now a generic service-mode abstraction**, not systemd-specific: it dispatches through a `detect_service_manager` step (systemd on Linux today; launchd on macOS is a planned but not-yet-implemented backend) and falls back to the existing background mode with a clear message on any host without a supported service manager, instead of only degrading silently on non-systemd Linux.
- **`install.sh` download failures** now print an actionable message (connectivity/firewall/no-release-yet) instead of a bare `curl`/`wget` error trace.

## [0.14.3] - 2026-07-06

### Changed
- **License: relicensed from MIT to Apache-2.0** (adds an explicit patent grant). Added a `NOTICE` file; every license reference (JSON-LD, `llms.txt`, README badge, site, docs footers) updated to match.
- Website, README, and `llms.txt` repositioned around the **self-hosted, multi-runtime inference control plane** (Ollama, vLLM, TGI, llama.cpp). Visible FAQ reconciled with the JSON-LD `FAQPage`; sitemap refreshed.
- `ROADMAP.md` rewritten as a technical, single-instance roadmap organized around the router-moat progression; pricing/commercial detail moved out of the public repo.
- Removed the internal `docs/design/` doc from the repository.

### Fixed
- **Warmup**: warming 2+ models on one node raced concurrent cold-load goroutines, and headroom accounting didn't see in-flight (not-yet-polled) loads, so a second model would falsely be evicted or never load.
- **Scheduler**: `fireSchedule` logged "fired" success even when the target node was missing or a warmup/unload schedule had zero models, making broken schedules indistinguishable from working ones. Now validates node existence + non-empty model list at creation/patch time (400), and logs skips explicitly.
- **Scheduler timezone mismatch**: schedules never fired for operators outside UTC — the scheduler evaluated `HH:MM` against the server/container clock (typically UTC in Docker), while the admin UI's time input implicitly meant the operator's local time. Added a persisted, configurable timezone; the scheduler and predictive prewarmer now evaluate against it, and the Warmup page shows a live server clock so operators can see exactly what time schedules are evaluated against.
- **HuggingFace model pull returning Bad Gateway**: the pull proxy waits for the whole download before responding; a hardcoded 5-minute client timeout killed any pull over 5 minutes (routine for multi-GB HF files). Raised to 2 hours.
- **Pinned models could still be unloaded**: the pin check only guarded auto-eviction, never the manual/scheduled unload path. Now blocked (409) on both, and the GPU Nodes page disables the unload button for pinned models.
- **Duplicate node registration**: nodes loaded from config.yaml, the DB store, and Docker discovery were merged with no URL-based dedup — only by exact name — so the same physical node could end up registered twice under different names, splitting its usage/eviction accounting.
- **`install.sh` self-discovery**: the port-8080 probe treated any 200 response on `/health` as a foreign llama.cpp node, so it discovered ollama-mesh's own admin API as a separate node; it now recognizes and skips itself via the unique `proxy_port` field, and skips the host's own IP during the subnet scan.
- **Sidebar/Dashboard version mismatch**: the sidebar showed a stale build-time version while the Dashboard showed the live one; the sidebar now fetches `/health` too.
- **Demo site server clock stuck on "Loading server time…"**: the static GitHub Pages demo (no backend) called the real `/admin/system-info` endpoint, which doesn't exist there, and swallowed the failure. It now returns mock system info like every other demo endpoint.

### Added
- **`SERVICE=1` install mode**: `install.sh` can now set up a systemd unit (`Restart=on-failure`) so ollama-mesh persists across reboots, in addition to the existing install-only and install+probe+run modes.
- **DCO (Developer Certificate of Origin) sign-off requirement** — CONTRIBUTING guidance, a PR-template checkbox, and a CI check.

## [0.14.2] - 2026-07-03

### Fixed
- Resolved 7 security, concurrency, performance, and UI issues surfaced by a codebase audit.
- Website polish: custom thin scrollbars on code/table blocks, code-block copy-button layout, mobile step layout, and split installer options with separate copy buttons.

## [0.14.1] - 2026-07-02

### Fixed
- GitHub Pages deploy pipeline — Actions workflow mode, tag-push handling, version derived from the git tag.
- `install.sh` subnet scan is now gated behind `PROBE=1`; `START=1` alone writes a localhost config without scanning.

## [0.14.0] - 2026-07-02

### Added
- **Persistent warm state** - the warm-model map is persisted and reconciled against live `/api/ps` on startup, so routing intelligence survives restarts.
- **`ollama-mesh bench`** - reproducible cold-vs-warm TTFT measured through the mesh proxy.
- **Weighted placement scoring** - multi-factor routing (warm / free VRAM / queue depth / health / success) with model pinning and post-failure node cooldown.
- **Predictive prewarming** - an in-memory transition ring buffer plus time-of-day patterns warm the next likely model before it is requested; accuracy metrics included.
- **Manual + scheduled model unload** - `POST /admin/nodes/{name}/unload` and a new "unload" schedule action.
- Installer subnet probing detects Ollama, vLLM, TGI, and llama.cpp nodes; optional Prometheus + Grafana provisioning.

### Changed
- Router decomposed into `placement` / `health` / `queue` behind interfaces (pure refactor, no behavior change).
- Go toolchain upgraded to 1.25.11 for standard-library security patches.

## [0.13.1] - 2026-07-02

### Fixed
- Admin login is now rate-limited per client IP to prevent brute-force.
- Hourly analytics are backfilled from SQLite on startup, so the dashboard shows continuous history after a restart.
- Docker auto-discovery uses the container's own network IP.

## [0.13.0] - 2026-07-01

### Added
- **Model lifecycle / LRU eviction** - eviction primitives plus operator-pinned never-evict models (B1), headroom-gated auto-eviction on the load path (B2), and a pinned-models control in the Warmup page (B3).

### Fixed
- Affinity deletions guarded under a write lock; `recover()` added to background goroutines.
- Warmup/schedule UI: edit/pause split, styling, and decluttering.

## [0.12.2] - 2026-07-01

### Added
- Scheduled warmup and a per-node warmup dashboard (W2 + W3).

## [0.12.1] - 2026-07-01

### Added
- Real per-node warmup with a keep-alive guard and a model-residency metric (W1).

### Changed
- Session affinity is gated behind `routing.session_affinity`; honest peer-monitor reporting; Anthropic overflow returns 501 where unsupported.
- Added the LIMITATIONS docs page; removed the unshipped SPCS section from the website.

### Security
- Block link-local / cloud-metadata node URLs (SSRF protection).
- Auth on by default, stronger key entropy, database file mode `0600`.

## [0.12.0] - 2026-07-01

### Added
- **Enterprise multi-user auth** - separate admin/user login paths, a user portal, and user management (approve, soft-delete, reset password).

### Security
- bcrypt-hashed passwords, DB-backed admin auth, and SQLite schema hardening.

## [0.11.1] - 2026-06-25

### Added
- Username/password login with sessions and change-password; installed-models view in the dashboard; API key name shown in the request log.

### Changed
- Auth on by default.

## [0.11.0] - 2026-06-25

### Added
- **SQLite persistence for all operational state.**
- **Node runtime auto-detection** (Ollama / vLLM / TGI / llama.cpp) with runtime badges in the UI.
- `vram_used_bytes` in the model catalog.

### Fixed
- Unique request IDs (fallback prevents empty/duplicate IDs); admin token validated before login, with a route guard.

## [0.10.0] - 2026-06-23

### Added
- **Multi-backend runtime support** - nodes now declare a `runtime:` field (values: `ollama`, `vllm`, `tgi`, `llamacpp`; default: `ollama`). The router is now runtime-agnostic via a `RuntimeProbe` interface in the new `internal/runtime` package.
- **RuntimeProbe interface** (`internal/runtime`) - defines `ListModels()` and `HealthCheck()` per backend. Implementations: Ollama (`GET /api/ps`), vLLM (`GET /health` + `GET /v1/models`), TGI (`GET /health` + `GET /info`), llama.cpp (`GET /health` + `GET /v1/models`).
- **Path-aware routing** - `/api/*` requests route exclusively to Ollama nodes. `/v1/*` requests route to any runtime. Non-Ollama backends that receive `/api/*` requests are skipped, not errored.
- **`runtime` field in admin API** - `GET /admin/nodes` now includes `runtime` on each node response.

### Changed
- Warmup (keepalive pings) skips non-Ollama nodes. Keep-alive is an Ollama-native feature; vLLM/TGI/llama.cpp do not expose it.
- VRAM reporting for vLLM, TGI, and llama.cpp nodes is not available via API. Use the existing `vram_total_mb` config field to declare capacity for these nodes. The dashboard labels the source as `declared`.

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
