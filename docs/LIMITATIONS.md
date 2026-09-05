# Known Limitations

This page documents what marbor does not do, what has been tested, and what to plan around in production. Infrastructure engineers evaluating the project for production use should read this before deploying.

---

## Encryption Key Loss

Secrets at rest (cloud provider API keys, runtime API keys, marbor-agent tokens, LiteLLM/HuggingFace/webhook credentials) are AES-256-GCM encrypted under a 32-byte master key (`MARBOR_ENCRYPTION_KEY` env var, or an auto-generated `marbor.db.key` file next to the database). **Losing that key is unrecoverable by design** - there is no way to decrypt the affected secrets without it.

If the key file is corrupted, marbor refuses to start with an explicit error - the safe case. If the key file is simply missing (deleted, or an env-var-only deployment lost the variable), marbor currently generates a new key silently instead of refusing to start, orphaning every previously-encrypted secret with no warning; the process still boots and keeps routing traffic, but affected secrets are silently dropped from list views or fail outright on lookup until re-entered. This gap is confirmed but not yet fixed.

Full blast-radius breakdown and the backup story: [`backup.md`](backup.md#the-encryption-key---back-it-up-like-the-database).

---

## Deployment Topology

### What has been tested
The validated topology is **one marbor process on a single host, routing to one or more remote Ollama nodes**. This includes bare-metal and EC2 deployments. Multi-node routing, failover, and cloud overflow have been exercised in this configuration.

No distributed or multi-instance marbor topology has been tested. There is no coordination layer, no distributed lock, and no leader election. Running two instances of marbor pointing at the same SQLite database file is not supported.

### No high availability or multi-region failover
marbor is a single process. If the host running it goes down, inference traffic stops until the process restarts. There is no hot standby, no floating IP handoff, and no automatic failover between marbor instances.

For production HA, the standard approach is to put a layer-4 load balancer (e.g., an AWS NLB) in front of two independent marbor instances, each with their own database file, and accept that in-flight requests to the failed instance are lost. This works because marbor is stateless for the routing path - only the admin session is lost on restart.

---

## Scale Ceiling (req/s and node count)

marbor's write path - the per-request `audit_log`, `request_log`, and `hourly_buckets`/`model_stats`
writes - is fully async via bounded 5000-slot buffered channels with drop-on-full
(`internal/audit/audit.go`, `internal/admin/admin.go`). Under sustained load, entries for a full
queue are dropped (and logged) rather than blocking requests. That queue-full point is the real
operational ceiling, and this section states it plainly. Full methodology, tooling
(`bench/loadtest`), and per-rate tables live in `docs/PRODUCTION.md`'s "Write-Path Capacity" section.

**req/s ceiling (single marbor process, single warm backend node, Windows dev workstation,
`bench/loadtest` sweep, re-measured 2026-09-05, superseding the 2026-08-13 figures which this
re-measurement found to be optimistic):**

| Signal | Result |
|---|---|
| Latency knee | ~400 req/s - p50 jumps from the 20-40ms baseline to 1.1-1.7s. Reproduced in two independent isolated runs, consistent with the original 2026-08-13 measurement. |
| First observed write-queue drop | As early as ~200-300 req/s (async logger / audit logger "queue full" lines), earlier than the 2026-08-13 figure of ~500 req/s. Real code changes landed on the write path between the two measurements (activity/audit merge, `internal/store` correctness fixes) - a plausible explanation for the shift, though this re-measurement ran on a shared, contended dev workstation with other concurrent processes and could not fully rule that out either. Treat ~200-300 req/s, not ~500, as the current honest single-node figure. |

**Node-count sensitivity:** measured with 2 and 4 real `cmd/mocknode` backend instances registered
against a single marbor process, same rate sweep. Result: **adding backend nodes did not raise the
req/s ceiling** - if anything, the tested 2-node and 4-node runs saturated the load generator earlier
than the 1-node baseline, but the marbor's own write-queue drop signal did not correspondingly worsen
(the 4-node run logged zero queue-full drops across the same rate range that reliably dropped
requests at 1 and 2 nodes). This points at the SQLite write path itself, not per-node backend
capacity, as the limiting factor - consistent with the original single-node measurement's framing - but the specific
degradation observed at higher node counts is confounded by this measurement running on a shared,
contended machine (more concurrent mocknode processes and marbor's own per-node polling loop compete
for the same CPU as the load generator). **Conclusion an operator can act on: node count is not a
lever for raising this ceiling; do not expect adding GPU nodes to increase write-path throughput.**
A precise linear/sub-linear verdict needs a re-run on a dedicated (uncontended) host.

**What this means operationally:** at the low hundreds of req/s, marbor's audit/request-log/stats
recording can start silently dropping entries (each drop is logged, never a request failure) well
before request-serving itself is affected. An honest small number is the point of this section - if
your fleet's real traffic approaches ~200 req/s sustained, budget for this and watch the marbor's own
log output for "queue full" lines rather than assuming headroom.

---

## Docker Node Auto-Discovery
Docker-based auto-discovery works by scanning the Docker socket for containers running Ollama and registering them as nodes. Discovered nodes use the container's own network IP (from the Docker API's `NetworkSettings`) when one is available, which is correct for containers on a bridge network regardless of whether marbor itself runs on bare metal or inside another container on the same Docker network.

**Fallback to `127.0.0.1`** still applies when a container has no network IP to report - the `--network host` case, where the container shares the host's network namespace and has no private IP of its own. In that case the discovered address is only reachable if marbor is also running with host networking (or directly on bare metal).

Workarounds if a discovered node is unreachable:
- Run marbor on the host directly (bare-metal or VM), not inside a container.
- Run marbor in Docker with `network_mode: host`, matching the discovered node's networking mode.
- Disable auto-discovery and add the node manually from the dashboard's **GPU Nodes** page, using the correct container IP or hostname.

---

## GPU Telemetry

### VRAM usage
Per-node VRAM usage (how much VRAM each model is consuming) is fetched from each node's `/api/ps` endpoint. That model-residency view is still available for every node, while richer remote GPU telemetry comes from the optional marbor agent when it is installed.

### VRAM capacity
Total VRAM capacity is read from `nvidia-smi` on the host running marbor (for NVIDIA GPUs). For Apple Silicon (MLX) nodes or remote nodes where `nvidia-smi` is not applicable or available, capacity must be declared explicitly when adding/editing the node from the **GPU Nodes** page. If neither is set, capacity is shown as `-` in the dashboard.

### Temperature and power draw
GPU temperature and power draw are read from `nvidia-smi` on the host running marbor for local NVIDIA GPU nodes.

For remote nodes, the same telemetry (temperature, power draw, fan speed, GPU model, CPU%, RAM, disk) is available via the marbor agent - a small, optional binary the operator installs on each remote GPU host. It detects NVIDIA (`nvidia-smi`), AMD (`rocm-smi`), Intel (`xpu-smi`), or Apple Silicon (`system_profiler`) automatically, whichever is present on that host. It is opt-in, not auto-deployed: marbor never pushes it to remote hosts on its own. Without the marbor agent installed, remote node telemetry gracefully degrades to show `-` for temperature and power draw in the dashboard.

For Apple Silicon (MLX) nodes, the marbor agent reports the chip model (e.g. "Apple M3 Max") but not temperature/power/fan - `system_profiler` doesn't expose those unprivileged, and Apple Silicon's unified memory has no separate VRAM figure to report. These show `-` in the dashboard rather than a guessed number. marbor enforces strict data honesty-we never substitute estimated or fabricated numbers for missing telemetry.

AMD and Intel GPU support via the marbor agent (`rocm-smi`/`xpu-smi` parsing) has not yet been validated against real hardware - if fan/temperature/power don't appear on an AMD or Intel node with the agent installed and running, that's the first thing worth reporting.

---

## Validated Runtime x GPU Vendor Matrix

marbor is designed for 5 runtimes (Ollama, vLLM, TGI, llama.cpp, MLX) x 4 GPU vendors (NVIDIA, AMD, Intel, Apple Silicon) - code-level support exists for all 5 runtime probes and all 4 GPU telemetry collectors. **Code existing is not the same as validated on real hardware.** This table states, per cell, what has actually been exercised against the real thing, so scope is stated honestly rather than implied by the feature list above.

| Runtime \ Vendor | NVIDIA | AMD | Intel | Apple Silicon |
|---|---|---|---|---|
| **Ollama** | VALIDATED - real deployment, real GPU, real workload. See the [TTFT benchmark](../README.md#ttft-performance-the-business-case) (v0.13.1, single consumer NVIDIA GPU, `bench/ttft.go`) and this page's Deployment Topology section. | UNTESTED - Ollama itself supports ROCm, but marbor's AMD GPU telemetry (`rocm-smi` parsing via the marbor agent) has not been exercised against a real AMD GPU. | UNTESTED - same gap as AMD: `xpu-smi` parsing is unvalidated against real Intel GPU hardware. | UNTESTED - Ollama runs natively on Apple Silicon; a Mac-mini heterogeneous-fleet run is planned but has not yet happened. |
| **vLLM** | UNTESTED - only exercised against a mock server (`cmd/mocknode`, matching vLLM's `/v1/models` and `/health` shapes) in the demo stack, never a real running vLLM instance. | UNTESTED - same mock-only coverage; vLLM's ROCm build has never been run against marbor. | UNTESTED - same mock-only coverage. | UNTESTED - vLLM has no official Apple Silicon (Metal/MPS) backend as of this writing; not marked UNSUPPORTED without direct confirmation, but real validation here is unlikely to be possible upstream. |
| **TGI** | UNTESTED - mock-only (`cmd/mocknode`), same as vLLM. | UNTESTED - mock-only. | UNTESTED - mock-only. | UNTESTED - mock-only; HuggingFace TGI has no native Apple Silicon build upstream, so real validation is unlikely to be possible. |
| **llama.cpp** | UNTESTED - mock-only (`cmd/mocknode`). | UNTESTED - mock-only. | UNTESTED - mock-only. | UNTESTED - mock-only, despite llama.cpp having a real, mature Metal backend for Apple Silicon - it just hasn't been run against marbor yet. |
| **MLX** | UNSUPPORTED - `mlx_lm.server` is Apple's own inference framework and only runs on Apple Silicon; there is no NVIDIA/CUDA build. | UNSUPPORTED - same reason: no ROCm build exists. | UNSUPPORTED - same reason: no Intel/oneAPI build exists. | UNTESTED - code exists (`internal/runtime/mlx.go`, request translation verified against `mlx_lm.server`'s documented SERVER.md schema) and is covered by unit tests against fixture responses, but has never been run against a real `mlx_lm.server` process on real Apple Silicon hardware. |

**Summary: 1 of 20 cells VALIDATED (Ollama + NVIDIA), 3 of 20 UNSUPPORTED (MLX on non-Apple vendors, by upstream design), 16 of 20 UNTESTED.**

Evidence basis for each marking:
- **VALIDATED (Ollama + NVIDIA):** [README.md's TTFT benchmark section](../README.md#ttft-performance-the-business-case) - a deployed marbor v0.13.1 routing to a real single consumer-GPU Ollama node, measured with `bench/ttft.go`.
- **UNTESTED (AMD/Intel GPU telemetry, any runtime):** CHANGELOG.md's marbor-agent GPU telemetry entry states plainly: "AMD and Intel support have not yet been validated against real hardware (built against each tool's publicly documented output format)."
- **UNTESTED (vLLM/TGI/llama.cpp, any vendor):** the only exercised counterparts are mock servers (`cmd/mocknode`, CHANGELOG: "Real vLLM/TGI/llama.cpp mock servers in the demo stack... verified against that code, not each project's full real API"). No CHANGELOG entry, commit, or test record describes running marbor against a real vLLM, TGI, or llama.cpp server.
- **UNTESTED (MLX + Apple Silicon):** MLX runtime support shipped as enum wiring and request translation verified against `mlx_lm.server`'s published SERVER.md schema (CHANGELOG), with unit-test coverage against fixture responses (`internal/runtime/detect_test.go`, `probe_test.go`) - not a real Apple Silicon run. The internal bench runbook's Mac-mini step is written but has not executed; the hardware-bench release gate remains BLOCKED on hardware access.
- **UNSUPPORTED (MLX on NVIDIA/AMD/Intel):** `mlx_lm.server` is Apple's own ML framework, built on Apple's Metal API; it has no CUDA, ROCm, or oneAPI build, so these three cells are not applicable rather than merely unexercised.

**Expanding coverage** (moving UNTESTED cells to VALIDATED) requires access to the real thing per cell - a running vLLM/TGI/llama.cpp server, a real AMD or Intel GPU host with the marbor agent installed, or Apple Silicon hardware for the Ollama and MLX Apple-Silicon cells. This is desirable but not required to state the matrix honestly; it is a separate, hardware-gated effort.

---

## Admin Dashboard Security

### Admin login lockout
The admin login endpoint throttles failed attempts per client IP: 5 failures within a 1-minute window trigger a 15-minute lockout (`429 Too Many Requests`, with a generic error that never reveals whether the username exists). A successful login clears the failure count for that IP. This state is in-memory and resets on process restart - an acceptable tradeoff since a meaningful brute-force run takes far longer than a typical restart cycle, and rotating credentials (not restarting the process) is the right response to a suspected compromise.

For defense in depth, still put a reverse proxy in front of the admin port (`8080`) and apply rate limiting there - nginx's `limit_req` directive or Cloudflare's rate limiting rules both work. The admin port should not be exposed to the public internet directly regardless.

Rate limiting on the proxy port (`11434`) is implemented per API key via a token bucket, separately from the admin login throttle above.

### Demo-mode auth bypass exists in the binary, but is not reachable in a real deployment
`admin.Server` has a `demoMode` flag that, when set, accepts a static `demo-session` bearer token in place of a real DB-backed session. It is set only by test code (`SetDemoMode` / `AdminToken`, called exclusively from `_test.go` files) - no CLI flag, config field, or environment variable in the shipped binary or `main.go` ever enables it, so a normally built and run `marbor` process has no code path that turns it on. (The public `/demo/` dashboard on the website is unrelated: it's a pure frontend flag, `VITE_FORCE_DEMO`, that makes the React app render entirely client-side mocked data - it never talks to a real `admin.Server` and never uses this token.) The flag stays in the shipped binary as dead code rather than being compiled out behind a build tag, since doing so cleanly requires reworking the ~20 test call sites that use it to authenticate against the real DB-backed session path instead.

---

## Data Persistence

### Analytics dashboard: traffic history restored, per-model breakdown still gaps after restart
Hourly traffic buckets (requests, tokens, local/cloud split, cost) are persisted to SQLite and now restored into the in-memory analytics store on startup, so the traffic charts show continuous history immediately after a restart instead of a dip.

The "By Model" table's Local/Cloud breakdown is not backfilled the same way. Per-model stats are persisted as an aggregate request count with no local/cloud split, but the dashboard needs that split - attributing the aggregate to either side on restart would fabricate a false 100%-local or 100%-cloud number, which violates the project's no-fake-data rule. So the by-model view still shows a gap that fills back in as new traffic arrives. No data is lost in either case - the SQLite records are intact.

### Routing rules persist to SQLite
Routing rules added via the admin API (the UI or `POST /admin/v1/routing/rules`) are written to SQLite and survive restarts.

---

## Configuration Model
marbor is DB-first: `marbor.db` (SQLite) is the sole source of truth for every setting - nodes, API keys, routing rules, cloud providers, and everything on the Settings page. There is no config file and no split-brain between a static file and runtime state.

If you manage marbor configuration via infrastructure-as-code (Ansible, Terraform, etc.), drive it through the `/admin/v1/...` REST API (the same endpoints the dashboard uses) from a post-deploy step, rather than templating a file.

---

## Session Affinity
Session affinity is implemented and gated by the `routing.session_affinity` flag. When enabled, requests carrying an `X-Session-ID` header are pinned to the same backend node (within `routing.session_affinity_ttl`) so the node's KV-cache context stays warm across turns; the pin falls back to normal routing if that node becomes unhealthy or the TTL lapses. When disabled (the default), routing is stateless and the header is ignored.

---

## Out of Scope
The following are deliberate non-goals, not gaps to be filled:
- **TLS termination.** marbor does not handle TLS. Put nginx or a load balancer in front for HTTPS. This keeps the binary simple and puts TLS configuration where operators already manage it.
- **Auto-deployed remote telemetry.** marbor agent must be manually installed per node - marbor does not auto-deploy it to remote hosts.
- **Multi-instance coordination.** No distributed consensus, no Raft, no etcd dependency. Single-host deployment only.
- **Chat UI, model fine-tuning, or web scraping.** marbor is a proxy and router. These are out of scope.
- **Cloud provider breadth.** OpenAI-compatible providers are supported via built-in presets (OpenRouter, Groq, Together, Fireworks, DeepSeek, Mistral, xAI, Cerebras, NVIDIA NIM) or a custom base URL; Anthropic gets native translation. LiteLLM's approach of abstracting hundreds of providers behind one client SDK is not a goal.
