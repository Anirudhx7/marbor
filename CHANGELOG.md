# Changelog

All notable changes to this project will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added
- **Node Agent (v1): real GPU/host telemetry for remote nodes, read-only, with a vendor-agnostic collector architecture and a self-installing persistent service.** A new `ollama-mesh agent` subcommand runs a small HTTP server on the GPU node itself, reporting temperature, fan speed, power draw, VRAM, CPU, RAM, and disk free via a versioned JSON schema (`GET /telemetry`) plus a Prometheus-compatible `GET /metrics` derived from it. Telemetry is collected by a background goroutine on a fixed interval (5s default, `--refresh-interval` to tune) into a cached snapshot - `/telemetry` and `/metrics` only ever read the cache, so nvidia-smi never runs on the request path regardless of poll frequency or how many things scrape the node. The payload carries a `last_updated` timestamp so a stale/stuck collector is visible rather than silently serving old numbers as if fresh. The mesh polls it on the existing node poll cycle - no new transport, no WebSockets/gRPC.
  - GPU collection is behind a `GPUCollector` interface (nvidia-smi is the first implementation, selected once at startup from a candidate list) so AMD ROCm, Apple Silicon, and Intel can be added later as new implementations without touching the agent's HTTP server, wire schema, or the mesh-side poller. A CPU-only node (or an unsupported vendor) gets an explicit "no GPU" result, never a fabricated reading.
  - The agent installs itself as a persistent, auto-restarting OS service - `ollama-mesh agent service install` registers with systemd (Linux), launchd (macOS), or a native Windows Service via `sc.exe` (Windows), no new dependencies, no manual unit files or `sc.exe` calls for the operator to write. Re-running install (e.g. after an upgrade, or to rotate the token) reconfigures and restarts the existing service in place.
  - The one-line install command generated from the GPU Nodes page (Linux/macOS via `install.sh`, Windows via the new `install.ps1`) now actually downloads the binary and runs the full service install, instead of assuming the binary already exists and running only in the foreground.
  - Each node gets its own opaque bearer token (AES-256-GCM encrypted at rest, distinct from the mesh's client-facing API keys - a leaked agent token can only feed fake telemetry to one node, not access inference or other secrets). Enable/regenerate/disable an agent from the GPU Nodes page; nodes without an agent installed keep showing `-` for these fields exactly as before, never a fabricated estimate.
  - Agents advertise a `capabilities` list (`["telemetry"]` today) instead of the mesh assuming every agent build supports every feature - future capabilities (models, diagnostics, actions, maintenance) get added to this list in the same change that implements them. Every `/telemetry` response also reports `agent_version`, `schema_version`, `platform`, `architecture`, `gpu_vendor`, and the locally-detected inference `runtime` (if any), surfaced in the GPU Nodes manage-agent modal and as Prometheus labels (`nodeagent_info`) - makes debugging a mixed-version, mixed-vendor fleet possible from the telemetry data alone.
  - **Protocol frozen for rolling upgrades.** Verified (with tests) that an older mesh and newer agents, or a newer mesh and older agents, can coexist in the same fleet indefinitely: unknown fields are always ignored on decode, and every field already treats "missing" as "unknown," never a fabricated zero. The mesh now logs once per node if an agent ever reports a newer `schema_version` than it understands, purely for operator visibility. This is the last planned change to the `/telemetry` wire format before real-world usage.
- **API key expiry is now editable after creation, with time-of-day precision.** Previously `expires_at` could only be set at key creation and had no field at all in the edit modal - once created, a key's expiry could never be added, changed, or cleared. `PATCH /admin/keys/{name}` now accepts `expires_at`, and both the create and edit forms use a combined date/time picker (was date-only on create) so expiry can be scoped to the hour/minute, not just the day.
- **Node Agent can now pull models locally on the node (`actions.pull_model`), and pull failures show the real reason.** Previously a model pull always went through the mesh proxying straight to the node's Ollama HTTP API, which had no way to pass a Hugging Face token to a gated/rate-limited download and was bound by the mesh's own outbound HTTP client timeout for a transfer it wasn't a party to - failures surfaced as a generic "Bad Gateway" with no indication of why. `POST /admin/v1/nodes/{name}/pull` now dispatches to the node's Node Agent when one is enabled and reports the new `actions.pull_model` capability: the agent runs the locally-detected runtime's own download mechanism (`ollama pull`, `text-generation-server download-weights` for TGI, `huggingface-cli download` for vLLM/llama.cpp), with the mesh's configured Hugging Face token injected into that one subprocess's environment only - never written to disk, never logged. Both this path and the pre-existing direct path now surface the real upstream/agent error text instead of a bare status code. Pulling a raw Hugging Face repo id (e.g. one copied from a model page, ending in `-GGUF`) into the free-text "Pull Model from Registry" field is also now auto-prefixed `hf.co/` to match the format Ollama actually requires, instead of failing outright. Nodes without an agent, or an agent build predating this capability, are unaffected.
- **Model pulls now show live, cancellable progress instead of a blocking spinner.** A pull request returns immediately and runs in the background, tracked by node+model; a browser-download-style widget (bottom-right, on every page) shows real bytes transferred, speed, and time remaining for the direct-to-Ollama path, or an elapsed-time indicator when the download only reports through the Node Agent (never a fabricated percentage). Pulling several models at once - across one node or several - shows one card per pull, each labeled with its node so they stay distinguishable. Cancelling a pull (with a confirm step) actually tears down the in-flight download on the node, not just the dashboard's view of it; a failed pull can be retried in place, and a finished one dismissed.
- **Manual "Ping warmup" button on the Warmup & Scheduling page.** `POST /admin/warmup/ping` (trigger a mesh-wide warmup pass on demand) previously had no UI caller - only reachable via a direct API call. A button next to the page title now triggers it and reports the result inline.
- **Schedules now show when they last ran.** Previously the only record of a warmup/unload/drain/undrain schedule firing was a server log line - an admin had no way to tell from the dashboard whether a schedule had actually run, or silently done nothing (e.g. pointed at a since-removed node). Each schedule row on the Warmup & Scheduling page now shows a "last ran Xh ago" / "never ran" indicator, red with the failure reason on hover if the last dispatch failed. This is live in-memory state (resets on mesh restart, same as other runtime signals) exposed via `last_run_at`/`last_status`/`last_error` on `GET/POST/PATCH /admin/schedules`.

### Fixed
- **GPU Nodes' CPU stat never showed a real number.** The admin API never serialized the Node
  Agent's collected host CPU utilization at all - `CPUPercent` was polled and stored on the
  node's in-memory state but silently dropped before reaching the JSON response, so the
  dashboard's CPU tile always rendered blank regardless of platform or agent status. It's now
  serialized (`cpuPercent`, gated by `agentPresent` the same way as fan speed/RAM/disk, `--`
  when no agent is reporting) and demo mode only shows a value on the one mock node that has an
  agent, matching how fan/RAM/disk are already demoed. The manage-agent modal copy also now
  mentions CPU usage alongside fan speed, RAM, and disk free.
- **Keep-warm models on a VRAM-constrained node flipped resident/evicted at random instead of settling on a predictable winner.** When two or more models were configured to always stay warm on the same node (via the Warmup page toggle) but didn't fit in VRAM together, the warm loop built its per-node model set from a Go map - whose iteration order is randomized every tick - so which model warmed last (and therefore survived LRU eviction) changed from one warmup cycle to the next. The "keep warm" list order is now treated as an explicit priority hierarchy (first in the list = highest priority): the warm set is built in list order and a higher-priority model can never be evicted to make room for a lower-priority one. The same model now reliably wins every time on a node that can't hold the full set, instead of thrashing. The Warmup page's "Models to keep warm" list is now an explicit, reorderable priority list (numbered rows with up/down controls, same pattern as the cloud-provider priority list in Settings) instead of an unordered toggle grid, so that priority is something you can see and set on purpose rather than an invisible side effect of click order.
- **The time-of-day scroll wheel in the schedule/expiry time picker was over-sensitive and janky on desktop.** Mouse-wheel input translated raw scroll delta directly into scroll position against a hardcoded row-height guess, so a single wheel notch could skip past several values, and snap-back animations could visibly stutter. Wheel input is now normalized to one value per notch (measured against actual row height), decoupled from touch/drag scrolling which still behaves natively.
- **System Audit Trail's action-type dropdown blurred the rows behind it when opened, on both desktop and mobile.** The desktop table wrapper was the only one in the app still using a translucent `bg-card/30 backdrop-blur-sm` background instead of the solid `bg-card` every other page's data table uses, and the mobile card view directly beneath the filter bar had the same translucent-plus-blur combination on each card - so an open dropdown panel bled a frosted-glass effect onto whatever sat underneath it. Both now use a solid `bg-card`, matching the rest of the app.
- **Mobile viewport pass across the whole admin UI.** Every page with a data table (Requests, System Audit, API Keys, GPU Nodes, Users, Routing, Analytics, Dashboard) previously fell back to raw horizontal table-scroll below 768px; each now gets a proper stacked card view with full action parity instead. The date/time picker used site-wide (filters, key expiry, schedules) could render its popup panel past the right edge of a narrow viewport with no clamping - it's now portaled and clamped to stay on-screen at any width. Several pages (Metrics, ModelAdvisor, Warmup, Settings) had individual flex/grid rows that didn't collapse to a single column on narrow screens and now do.
- **Status/action badges (System Audit, GPU Nodes, Requests) used dark-mode-only accent colors** (e.g. bare `text-amber-400`) with no light-mode variant, so they lost contrast against a light background. Now theme-aware (`text-amber-600 dark:text-amber-400` pattern) like the rest of the UI.
- **Past dates were selectable in the API key expiry picker**, only rejected after a round-trip to the server. The picker now grays out and disables any day before today.
- **A key's `expires_at` was silently dropped on mesh restart.** It was applied in-memory at creation but never written to `mesh.db` (`runtime_keys` had no `expires_at` column) and never re-loaded into the auth middleware at boot - a key created with an expiry effectively lost it the next time the mesh restarted.

### Security
- **Secrets are now encrypted at rest in `mesh.db`.** Cloud provider API keys, mesh-issued API keys, the LiteLLM key, HuggingFace token, and webhook secret were previously stored as plaintext columns/settings - readable by anything with access to the SQLite file (backups, misconfigured storage, a copied `.db`). They're now AES-256-GCM encrypted, with the key held in a separate `mesh.db.key` file (0600) generated on first boot, or supplied via `MESH_ENCRYPTION_KEY` (base64 32-byte key) for operators who want to manage it themselves. Existing installs migrate transparently on first boot after upgrading - no manual step, no re-entering keys.

### Removed
- **Peer health monitor (`ha` config block, `GET /admin/ha/peers`, "Peer Health Monitoring" Settings card) removed.** It only ever polled other ollama-mesh instances' `/health` endpoints for passive observability - no failover, no shared state, no leader election, and it never provided real HA despite the name. ollama-mesh is a single-instance control plane by design (see Architecture Laws); this module's premise (multiple mesh peers) doesn't fit that model, and Node Agent already covers the real multi-machine need (one mesh, many GPU nodes). Anyone who had `ha.enabled: true` set can safely drop it - the `ha` config block and its `ha_*`/`ha_peers` settings keys are silently ignored (not read, not migrated).

## [0.16.0] - 2026-07-16

### Added
- **Custom Select, date picker, and time picker components across the whole UI**, replacing native browser `<select>`/date/time inputs - consistent theming, no more browser-default styling breaking out of the dark UI, dropdowns that flip position instead of clipping out of modals, and styled native checkboxes/number-input spinners.
- **Admin-configurable audit log retention** (`Settings` → Global Warmup & Audit → "Audit Log Retention (days)"). Replaces a hardcoded "keep the last 10,000 rows" trim that ran a `DELETE` on every single proxied request. Now a periodic sweep (startup + every 12h) removes `audit_log` rows older than the configured window; set to `0` to keep every entry forever instead.
- **Separate, independent retention setting for the System Audit page** (admin action trail - who changed what) via "System Audit Retention (days)", next to the request-log one. This table previously had no pruning at all. Defaults to `0` (forever) rather than the request log's 30 days, since it's low-volume and security-sensitive.
- **Requests page key/node filters are now a searchable, autocompleting list** (backed by the mesh's actual current API keys/nodes) instead of free text requiring an exact guess, and a precise custom "From" date/time filter joins the existing "Until" one instead of only three canned lookback presets. Every text/date filter box also gets an inline clear (×) once it has a value.
- Cloud fallback providers now try in operator-defined priority order (editable via up/down reorder controls in Settings), falling through to the next provider on connection failure instead of always using the same one.
- LiteLLM integration, when enabled, now takes over cloud fallback entirely - the per-provider priority list is disabled in the UI and ignored at request time, since LiteLLM already manages its own provider ordering and retries.
- **Anthropic cloud fallback provider actually works now.** Selecting "Anthropic" as a cloud provider previously reached a hardcoded 501 for `/api/chat`, `/api/generate`, and `/v1/chat/completions` because those get translated to an OpenAI-shaped path (`/v1/chat/completions`) that Anthropic doesn't expose. The mesh now translates requests to Anthropic's native `/v1/messages` schema (system prompt pulled out of `messages[]`, `max_tokens` required field, `x-api-key`/`anthropic-version` auth instead of `Authorization: Bearer`) and translates the response back to OpenAI shape - streaming included - so it flows through the existing cloud-fallback and Ollama-NDJSON pipeline unchanged. Embeddings still 501s (Anthropic has no equivalent).

### Fixed
- **The "Test Key" button for cloud providers (Anthropic, OpenRouter) accepted garbage keys.** The test call was a no-op that always reported success regardless of whether the key actually worked - it now genuinely gates on the provider's own auth response.
- **Requests page key/node filters silently returned zero rows on partial input.** They matched by SQL exact-equality while the model filter used substring `LIKE` - typing anything but the full, exact key/node name looked broken. Both now match by substring, consistently with model.
- **`system_audit_log` (System Audit page) grew completely unbounded** - unlike the request audit log, it had no row cap or pruning of any kind. Now covered by the periodic prune above.

### Changed
- **BREAKING: `config.yaml` is gone. ollama-mesh is now fully DB-first (SQLite/`mesh.db`).** A fresh install starts blank-slate: the binary opens/creates `mesh.db`, prints a banner pointing at the dashboard, and everything - nodes, API keys, ports, routing knobs, Docker auto-discovery, HA peer monitoring, webhooks, thermal watchdog, cloud providers, model context windows - is configured from the admin dashboard (`admin`/`admin` default login, forced password change on first login) or the admin REST API. `main.go` no longer has `--config`/`CONFIG_PATH`/`-validate`; it has `--db` (SQLite path, default `mesh.db`, or `MESH_DB_PATH` env) and a new `--seed-node "name=...,url=...,runtime=..."` (repeatable) that writes a node directly to the DB and exits, used by `install.sh`'s discovery wizard. Config hot-reload (`kill -HUP` / `POST /admin/v1/config/reload`) now re-syncs live state from SQLite instead of re-reading a file. `Settings.tsx` gained full parity coverage for every field that previously had no UI (~20 fields) plus an editable Cloud Providers CRUD table and a Model Context Windows editor.
- **`install.sh`'s network probe is now the default flow, not opt-in.** After install, it always scans the local subnet for Ollama/vLLM/TGI/llama.cpp, prints a numbered list of what it found, and interactively prompts which to add (`comma-separated numbers`, `all`, or `skip`) - selected nodes are seeded via `--seed-node`. No config file, API key, or admin-token generation happens in `install.sh` anymore. `FORCE_PROBE=1` re-runs the wizard against an existing `mesh.db`.
- **`uninstall.sh`** no longer references `config.yaml` (`KEEP_CONFIG` env var removed) - only `mesh.db`, the pidfile, systemd unit, and log file are ever touched.
- `config.example.yaml` deleted (no file-based config left to document).

## [0.15.1] - 2026-07-14

### Changed
- **Model configuration overrides are now keyed by `(model, node)` instead of model alone.** The same model name can be resident on nodes with different runtimes (Ollama/vLLM/TGI/llama.cpp) or simply different VRAM budgets, and a single shared-by-model-name profile couldn't express either case. Config resolution moved from before routing to right after node selection (it can't be known which node's profile applies until a node is actually chosen). New `GET /admin/model-config/capabilities` endpoint is the single source of truth - for each runtime, exactly which fields take effect - read by both the injection code and the UI's field filtering, so they can't drift apart. The Advanced Settings modal keeps one card per model with a node-selector inside it.
- **The model-config parameter matrix was corrected and substantially expanded**, verified against each runtime's actual current source/API (not memory or older docs): removed 7 fields that don't exist in current Ollama (`flash_attention`, `offload_kv_cache_to_gpu`, `rope_frequency_base/scale`, `use_mlock`, `tensor_parallelism`, `tfs_z`) - these were being silently dropped by Ollama's own server on every request. Added the 3 real Ollama fields that were missing (`num_keep`, `main_gpu`, `draft_num_predict`). Expanded llama.cpp from 7 to 17 real extra fields (DRY/XTC repetition control, `logit_bias`, `n_probs`, `min_keep` - its server README confirms these are accepted on its OpenAI-compatible endpoint too) and vLLM from 3 to 10 (`length_penalty`, `stop_token_ids`, `min_tokens`, etc.) via a new "Runtime-Specific Extras" UI section.
- `Reset this node to defaults` and the equivalent destructive actions across the admin UI (suspend/delete/reset-password on Users; drain/toggle-predictive-prewarm on GPU Nodes; routing strategy change; schedule delete; toggle predictive warmup engine; config reload) now confirm before firing, matching the pattern already used for Remove Node/Unload Model/Revoke API Key.

### Fixed
- **`system` prompt override wasn't actually applied for vLLM/TGI/llama.cpp** - only Ollama ever received it. Now prepended as a leading `{"role":"system",...}` chat message for every runtime, never overwriting a client-supplied system message.
- **`install.sh`'s network probe misidentified the mesh's own admin port as a TGI backend.** Two bugs: the TGI check only verified an HTTP 200 on `/info` rather than the response content, and the mesh's own admin server serves its embedded dashboard as a catch-all for any unmatched path - so any ollama-mesh instance (including itself) false-positived as a TGI node. Separately, the self-IP skip list broke on any host with more than one local network interface (e.g. any Docker host), so the subnet scan probed the mesh's own LAN IP anyway.
- **The "Reset password" action on the Users page fired the actual password reset (revoking all sessions) immediately on click**, before any confirmation - the confirm-looking modal only showed the result afterward. Now gated behind a real confirm step first.

### Added
- **Real vLLM/TGI/llama.cpp mock servers in the demo stack** (`cmd/mocknode`, renamed from `cmd/mockollama` since it's no longer Ollama-only - one binary, a `RUNTIME` env var selects the personality). Each implements exactly what the mesh's own runtime-detection/health probes call, verified against that code, not each project's full real API. `make demo` now brings up all 5 backend nodes (2× Ollama, vLLM, TGI, llama.cpp) by default.

## [0.15.0] - 2026-07-14

### Added
- **Advanced model configuration overrides**: operators can now set a persisted default parameter profile per model - all 30 Ollama/LM-Studio-class knobs across load-time/engine (`num_ctx`, `num_gpu`, `flash_attention`, `num_batch`, `num_thread`, `use_mmap`/`use_mlock`, RoPE, `ttl`, tensor parallelism), inference-time sampling (`temperature`, `top_p`, `top_k`, `min_p`, `typical_p`, `tfs_z`, `max_tokens`, `seed`, `stop`, repeat/presence/frequency penalties, mirostat, `response_format`), and meta/orchestration (`system`, `template`, per-model `rpm`/`tpm` caps). New `GET/PUT/DELETE /admin/model-config` + `GET /admin/model-configs` endpoints back an "Advanced Settings" modal wired into Models, GPU Nodes, and Model Advisor. Inference-time defaults are injected into outgoing requests only where the client didn't already specify them (works across all runtimes via `/v1/*`); load-time/engine params apply only on Ollama-native `/api/*` requests and are visibly disabled in the UI for models resident on vLLM/TGI/llama.cpp nodes, since those runtimes take such settings only as launch-time flags, not per-request. `flash_attention`, `offload_kv_cache_to_gpu`, `tensor_parallelism`, and `logit_bias` are stored/exposed but not yet injected anywhere (no per-request hook exists in any supported runtime today).
  > Superseded in 0.15.1 above: this profile shape and field list were reworked (keyed by `(model, node)`, and the field list corrected against each runtime's real current API) the same day.
- **Requests page filters**: added `node`, `status` (success/client_error/server_error category), and `until` (upper time bound) filters to the server-side `/admin/audit` endpoint and Requests page filter toolbar, alongside the existing model/key/cloud/since filters.
- **Dismiss button on the cloud-spend warning banner** - closes for the current session instead of staying pinned until the underlying spend drops below the threshold.
- **`uninstall.sh`**: removes the binary, the systemd service (if installed), and a background (nohup) instance; prompts before deleting `config.yaml`/`mesh.db` (kept by default, including in non-interactive/piped runs - `KEEP_DB=0`/`KEEP_CONFIG=0` to remove without prompting).
- **`install.sh` post-install health checks**: after starting (either mode), validates `config.yaml` (`-validate`), confirms the proxy/admin/metrics ports are actually responding (not just that the process exists), and reports reachability of configured backend nodes.
- **`install.sh` upgrade reporting**: prints old → new version on reinstall/upgrade via the binary's own `-version` flag; warns when the binary on disk was upgraded but the running background process hasn't been restarted yet.
- **`install.sh` idempotent background start**: re-running the installer while a `nohup`-started instance is already running no longer starts a competing second process - it detects the existing one (via a new `ollama-mesh.pid`) and re-verifies its health instead. A stale pidfile (process no longer running, e.g. after a crash) is cleaned up and a fresh instance starts normally.

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
- **Scheduler timezone mismatch**: schedules never fired for operators outside UTC - the scheduler evaluated `HH:MM` against the server/container clock (typically UTC in Docker), while the admin UI's time input implicitly meant the operator's local time. Added a persisted, configurable timezone; the scheduler and predictive prewarmer now evaluate against it, and the Warmup page shows a live server clock so operators can see exactly what time schedules are evaluated against.
- **HuggingFace model pull returning Bad Gateway**: the pull proxy waits for the whole download before responding; a hardcoded 5-minute client timeout killed any pull over 5 minutes (routine for multi-GB HF files). Raised to 2 hours.
- **Pinned models could still be unloaded**: the pin check only guarded auto-eviction, never the manual/scheduled unload path. Now blocked (409) on both, and the GPU Nodes page disables the unload button for pinned models.
- **Duplicate node registration**: nodes loaded from config.yaml, the DB store, and Docker discovery were merged with no URL-based dedup - only by exact name - so the same physical node could end up registered twice under different names, splitting its usage/eviction accounting.
- **`install.sh` self-discovery**: the port-8080 probe treated any 200 response on `/health` as a foreign llama.cpp node, so it discovered ollama-mesh's own admin API as a separate node; it now recognizes and skips itself via the unique `proxy_port` field, and skips the host's own IP during the subnet scan.
- **Sidebar/Dashboard version mismatch**: the sidebar showed a stale build-time version while the Dashboard showed the live one; the sidebar now fetches `/health` too.
- **Demo site server clock stuck on "Loading server time…"**: the static GitHub Pages demo (no backend) called the real `/admin/system-info` endpoint, which doesn't exist there, and swallowed the failure. It now returns mock system info like every other demo endpoint.

### Added
- **`SERVICE=1` install mode**: `install.sh` can now set up a systemd unit (`Restart=on-failure`) so ollama-mesh persists across reboots, in addition to the existing install-only and install+probe+run modes.
- **DCO (Developer Certificate of Origin) sign-off requirement** - CONTRIBUTING guidance, a PR-template checkbox, and a CI check.

## [0.14.2] - 2026-07-03

### Fixed
- Resolved 7 security, concurrency, performance, and UI issues surfaced by a codebase audit.
- Website polish: custom thin scrollbars on code/table blocks, code-block copy-button layout, mobile step layout, and split installer options with separate copy buttons.

## [0.14.1] - 2026-07-02

### Fixed
- GitHub Pages deploy pipeline - Actions workflow mode, tag-push handling, version derived from the git tag.
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
- saved_usd and cloud_spent_usd return JSON null (shows "-" in UI) when requests exist but no token data was parseable
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
