# ollama-mesh

**The self-hosted control plane for AI inference - warm-aware GPU routing, an OpenAI-compatible gateway, and cost-metered cloud overflow for Ollama, vLLM, TGI, llama.cpp, and MLX**

One OpenAI-compatible endpoint for all your self-hosted LLM traffic. ollama-mesh routes every request to the GPU node that already holds the model warm in VRAM - across Ollama, vLLM, TGI, llama.cpp, and MLX (Apple Silicon) - turning your own hardware into a high-availability alternative to cloud LLM APIs. Bearer-token authentication and per-key rate limits protect your GPUs; cloud overflow to OpenAI or Anthropic activates only when local capacity is fully saturated, with real-time financial tracking. Local hardware first. Cloud second. Full spend attribution.

[![Build Status](https://github.com/Anirudhx7/ollama-mesh/actions/workflows/ci.yml/badge.svg)](https://github.com/Anirudhx7/ollama-mesh/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Anirudhx7/ollama-mesh?include_prereleases)](https://github.com/Anirudhx7/ollama-mesh/releases/latest)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

![ollama-mesh dashboard](website/screenshots/dashboard.png)
*Enterprise dashboard: live request telemetry, cluster-wide VRAM utilization, per-key cost attribution, and cloud-deflection savings - all from real parsed token counts.*

---

## Quick Start

### Try it in 5 minutes (No GPU/Ollama required)

Experience the complete gateway and monitoring stack locally in 5 minutes using mock backends:

1. **Clone and start the demo stack**:
   ```bash
   git clone https://github.com/Anirudhx7/ollama-mesh && cd ollama-mesh
   make demo
   ```
   This spins up `ollama-mesh`, two mock Ollama backend nodes, Prometheus, and Grafana, then runs a 20-request benchmark to generate live telemetry.

2. **Access the dashboards**:
   * **ollama-mesh Dashboard**: [http://localhost:8080](http://localhost:8080) (Credentials: `admin` / `admin`)
   * **Grafana Telemetry**: [http://localhost:3000](http://localhost:3000) (Pre-configured dashboard included)

3. **Run a manual benchmark**:
   Test the cold-vs-warm latency gap through the mesh proxy:
   ```bash
   go run ./cmd/bench --target http://localhost:11434
   ```

4. **Clean up**:
   ```bash
   make demo-down
   ```

---

### Quick Installer (Linux & macOS)

*   **Install only**
    ```bash
    curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | sh
    ```
    Downloads the official matching binary for your platform (`linux`/`darwin` and `amd64`/`arm64`) and installs it to `/usr/local/bin`. Run `ollama-mesh` manually to start. If a version is already installed, this reports old → new instead of upgrading silently.

*   **Quick demo - Auto-Discover & Run in background**
    ```bash
    curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | START=1 sh
    ```
    Installs the binary, starts the gateway in the background against a fresh `mesh.db`, and prints operational access details. Before starting, it scans the local physical network subnet (and localhost) for active GPU backends (Ollama, vLLM, TGI, and llama.cpp) and interactively prompts you to pick which discovered nodes to seed into `mesh.db` (comma-separated numbers, `all`, or `skip`) - there's no config file to hand-edit. This starts a plain background process (`nohup`) - it won't survive a reboot, so treat this as a way to try ollama-mesh, not run it long-term. After starting, the installer verifies the proxy, admin dashboard, and metrics endpoints are actually responding (not just that the process exists) and prints diagnostics if anything's off. Re-running this command while an instance is already running won't spawn a duplicate - it detects the existing process and re-verifies its health instead.

*   **Production - Auto-Discover & Run as a managed service (recommended for real deployments)**
    ```bash
    curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | SERVICE=1 sh
    ```
    Same as the quick-demo command (including the interactive node-discovery prompt), but instead of a background process it installs and enables a proper OS service (`Restart=on-failure`, starts on boot) - this is what you want for anything you intend to keep running. Currently implemented via `systemd` on Linux (requires root/sudo; logs via `journalctl -u ollama-mesh -f`). `SERVICE=1` is deliberately OS-agnostic - on macOS or any host without a supported service manager, it prints a notice and falls back to the same background mode as the quick-demo command rather than failing the install.

### Uninstalling

```bash
curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/uninstall.sh | sh
```

Run this from the same directory `install.sh` was run in (it looks for `mesh.db` and the pidfile there). It stops and removes the systemd service or background process and removes the binary. `mesh.db` is always kept by default when piped like this (stdin isn't a terminal, so the keep/remove prompt never runs) - pass `KEEP_DB=0` to remove it instead:

```bash
curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/uninstall.sh | KEEP_DB=0 sh
```

To get the interactive `Keep SQLite database? [Y/n]` prompt instead of relying on the env vars, download the script first so it runs with a real terminal attached: `curl -fsSL .../uninstall.sh -o uninstall.sh && sh uninstall.sh`.

---

### Docker Compose (Production Deployment)

Run a production-ready gateway + metrics stack scraping the proxy:
```bash
git clone https://github.com/Anirudhx7/ollama-mesh && cd ollama-mesh
docker compose up -d
```
This starts:
* **ollama-mesh** ([http://localhost:8080](http://localhost:8080)): Main gateway container.
* **Prometheus**: Automatically scraping the mesh metrics endpoint.
* **Grafana** ([http://localhost:3000](http://localhost:3000)): Pre-provisioned with the official [ollama-mesh dashboard](grafana/ollama-mesh.json).

---

## The Problem: Uncontrolled LLM Cloud Spend at Scale

Enterprise teams deploying LLM-powered applications - coding agents, RAG pipelines, internal copilots - face a compounding cost problem:

- **Cold-start latency tax.** Generic load balancers spray requests across GPU nodes with no awareness of model residency. Each miss triggers a 15–45 second model load from disk to VRAM, destroying time-to-first-token (TTFT) SLAs.
- **Invisible cloud egress.** Without a local-first routing layer, traffic silently overflows to OpenAI/Anthropic at $0.15–$60/M tokens. Platform teams discover the bill at month-end.
- **No GPU utilization visibility.** Ops teams have Grafana for CPU and memory. They have nothing for per-node VRAM residency, model warm state, or inference cost attribution across API keys.

**ollama-mesh eliminates all three.** It sits between your applications and your GPU fleet, routing every request to the node that already has the model loaded in VRAM. Cloud overflow is explicit, metered, and off by default. Every token is counted, attributed to an API key, and valued against your configured cloud reference rate.

---

## Core Architecture

```
Client Application (Agent / RAG / Copilot)
    │
    ▼
┌───────────────────────────────────────────────────────┐
│  ollama-mesh endpoint (:11434)                        │
│                                                       │
│  Auth ─► Rate Limit ─► Quota Check ─► Model Allow     │
│    │                                                  │
│    ▼                                                  │
│  Request Queue (configurable depth + backpressure)    │
│    │                                                  │
│    ▼                                                  │
│  Router: extract model from JSON body                 │
│    ├── Warm in VRAM? ──► Route to warm node           │
│    │   (least-connections among warm candidates)      │
│    ├── VRAM-fit placement ──► Node with most headroom │
│    ├── Session affinity (X-Session-ID) ──► KV-cache   │
│    └── All busy/down? ──► Cloud fallback              │
│         (OpenAI/Anthropic, format-translated)         │
│                                                       │
│  Token Tracking ─► Cost Attribution ─► Audit Log      │
└───────────────────────────────────────────────────────┘
    │               │              │
    ▼               ▼              ▼
  GPU Nodes     Cloud APIs     Prometheus :9090
  (Ollama/      (overflow)     Grafana Dashboard
   vLLM/TGI/
   llama.cpp/MLX)
```

**Single static Go binary. Zero runtime dependencies. No Python. No JVM. No Node.js.**

---

## Enterprise Feature Matrix

| Category | Feature | Detail |
|----------|---------|--------|
| **GPU-Aware Routing** | Warm-first model routing | Polls `/api/ps` on every node every 2s. Routes to the node where the model is already resident in VRAM. Eliminates cold-start latency. |
| | VRAM-fit placement | Cold requests route to the node with the most free VRAM. Prevents OOM under concurrent multi-model traffic. |
| | Session affinity (KV-cache) | `X-Session-ID` header pins a conversation to a node. KV-cache stays hot - subsequent turns skip re-prefill. TTL-based eviction. |
| | Proactive model warmup | `keep_alive` pings on a configurable schedule keep priority models resident between requests. |
| **Financial Controls** | Real-time savings tracking | Every locally-served token valued against your cloud reference rate. Dashboard shows exact dollar savings vs pure-cloud baseline. |
| | Per-key cost attribution | Token totals and estimated cost per API key per month. Attribute inference spend to teams, projects, or agents. |
| | Cloud spend metering | Overflow tokens priced at provider-configured rates. Full local-vs-cloud cost breakdown. |
| | Per-key quotas | Hard `daily_limit`/`monthly_limit` per key. 429 when exceeded. Persisted across restarts. |
| **Multi-Tenant Auth** | Per-key rate limiting | Token-bucket rate limiter per API key. `X-RateLimit-Limit/Remaining/Reset` headers on every response. |
| | Model allow-lists | Per-key model restrictions. 403 on unauthorized model access - enforced at the control plane, not advisory. |
| | Key expiration | `expires_at` per key. Automatic invalidation. No manual rotation under pressure. |
| **Observability** | Prometheus metrics | 14 production metrics: request throughput, latency percentiles, active connections, token counts, cache hit/miss, retry rates, cloud fallback frequency, quota rejections, request queue depth/timeouts, warmup pings, panic recovery, node health. |
| | Grafana dashboard | Included JSON (`grafana/ollama-mesh.json`). One-click import. VRAM utilization, request throughput, latency percentiles, cloud fallback rate. |
| | Structured logging | `--log-format json` for Loki, Datadog, Fluentd, Splunk. Per-request access log with key name, model, node, status, latency, request ID. |
| | Audit trail | Append-only JSON-lines audit log. Every request recorded with crypto/rand request IDs. |
| | Webhook alerts | `node_down`/`node_up` events with HMAC-SHA256 signatures. PagerDuty/OpsGenie/Slack-ready. |
| **Resilience** | Automatic retry/failover | Dead node before first byte triggers retry on alternate healthy nodes → cloud → 502. Transparent to the client. |
| | Request queue | Configurable `queue_max_depth` and `queue_timeout_ms`. Traffic spikes queue and drain rather than immediately 502-ing. |
| | Node drain | `POST /admin/nodes/{name}/drain` marks a node so the router skips it for new requests while in-flight work completes. Zero-downtime GPU maintenance. |
| | Config hot-reload | `SIGHUP` or `POST /admin/v1/config/reload` re-reads config in place. Key rotations and routing changes take effect without dropping connections. |
| **Cluster Telemetry** | Cluster-wide VRAM | Per-node used-VRAM live across the entire cluster from each node's own `/api/ps`. No sidecar agent required. |
| | GPU metrics | nvidia-smi integration on mesh host: temperature, power draw, total capacity. Remote nodes: real telemetry via the optional Node Agent, or operator-declared `vram_total_mb` if it is not installed. Every figure labelled with its source (nvidia/api/declared/agent). |
| | VRAM fit indicators | Green/yellow/red badges per model per node. Ops teams see at a glance whether a model fits in available VRAM. |
| **Multi-Backend** | Ollama, vLLM, TGI, llama.cpp, MLX | Declare `runtime: ollama/vllm/tgi/llamacpp/mlx` per node. The router is runtime-agnostic; health probes and model-list calls use the correct API per runtime. |
| | Path-aware routing | `/api/*` routes to Ollama nodes only. `/v1/*` routes to any runtime. Non-Ollama nodes are transparent to OpenAI SDK clients. |
| **Deployment** | Single binary | One static Go binary per platform. Drop onto a VM and run. No package manager, no virtualenv, no container runtime required. |
| | Docker auto-discovery | Scans Docker socket for `ollama/ollama` containers. Auto-registers nodes. Zero config. |
| | Cloud format translation | Ollama-native requests that overflow to cloud get OpenAI responses translated back to Ollama NDJSON. Clients never see a format difference. |

---

## TTFT Performance: The Business Case

The single most impactful metric for LLM infrastructure is **Time-to-First-Token (TTFT)**. Every cold model load adds tens of seconds of latency before the first token appears. In a multi-agent workflow making hundreds of calls per hour, this compounds into minutes of wasted wall-clock time per pipeline execution.

ollama-mesh's warm-first routing avoids this: the router knows which models are resident in VRAM on which nodes at sub-3-second granularity and sends each request to a node that already has the model loaded.

### Measured numbers (real hardware, not estimates)

Measured through a deployed ollama-mesh v0.13.1 instance routing to a single consumer-GPU
Ollama node, using [`bench/ttft.go`](bench/). Model: an 8B-parameter Q4_K_M model
(~9.6 GB on disk). Cold = model evicted from VRAM (`keep_alive: 0`) before each request;
warm = model already resident.

| Scenario (via mesh) | n | p50 TTFT | min | max |
|---|---|---|---|---|
| Cold (model must load from disk) | 3 | **17.3 s** | 11.5 s | 18.1 s |
| Warm (model resident) | 10 | **8.1 s** | 1.9 s | 13.8 s |

Fastest warm sample observed through the mesh: **0.4 s** - a 43× improvement over the
median cold start.

Honest context for these numbers: on the benchmark node only ~3.3 GB of the model's
~10.6 GB runtime footprint fit in VRAM, so even "warm" first-token latency was partly
CPU-bound and jittery. On a node where the model fully fits in VRAM, the warm path is
the GPU's native prompt-eval speed and the cold-vs-warm gap widens further. A control
run direct-to-node (bypassing the mesh) showed the same warm-latency profile, i.e. the
mesh's proxy overhead is negligible. Reproduce it on your own hardware with the
harness in [`bench/`](bench/).

---

## The Savings Angle

This is the dashboard screenshot that sells itself: ollama-mesh tracks every token you served locally vs in the cloud, and shows you exactly how much that local inference saved compared to routing everything to OpenAI.

The math uses real parsed token counts from each response (`eval_count` from Ollama, `usage.total_tokens` from cloud), valued at your configured reference rate. When token data is unavailable, the dashboard shows "-" rather than a fabricated number. No fake math.

Platform engineers with a team routing through local GPU hardware typically see $200–$3,000+/month in avoided cloud spend visible in the dashboard within the first week. Full financial model: [SAVINGS-MATH.md](docs/SAVINGS-MATH.md).

---

## Supported Backends

ollama-mesh is runtime-agnostic. Declare `runtime:` per node and the router uses the correct health probe and model-discovery call for each backend.

| Backend | `runtime:` value | Health check | Model discovery | Path routing |
|---------|-----------------|--------------|-----------------|--------------|
| Ollama | `ollama` (default) | GET /api/ps | /api/ps response | /api/* and /v1/* |
| vLLM | `vllm` | GET /health | GET /v1/models | /v1/* only |
| TGI (HuggingFace) | `tgi` | GET /health | GET /info | /v1/* only |
| llama.cpp server | `llamacpp` | GET /health | GET /v1/models | /v1/* only |
| MLX (`mlx_lm.server`, Apple Silicon) | `mlx` | GET /v1/models | GET /v1/models | /v1/* only |

`/api/*` paths (Ollama-native) route only to Ollama nodes. `/v1/*` paths route to any runtime - OpenAI SDK clients work unchanged against a mixed fleet.

**Mixed-fleet configuration payload (JSON structure for `POST /admin/v1/nodes` or dashboard config):**

```json
[
  {
    "name": "ollama-local",
    "url": "http://localhost:11434",
    "runtime": "ollama"
  },
  {
    "name": "vllm-gpu",
    "url": "http://10.0.1.20:8000",
    "runtime": "vllm",
    "vram_total_mb": 81920
  },
  {
    "name": "tgi-server",
    "url": "http://10.0.1.21:8080",
    "runtime": "tgi"
  },
  {
    "name": "llamacpp-server",
    "url": "http://10.0.1.22:8080",
    "runtime": "llamacpp"
  },
  {
    "name": "mlx-mac-studio",
    "url": "http://10.0.1.23:8080",
    "runtime": "mlx"
  }
]
```

---



**Supported platforms** (single static binary per target + multi-arch Docker image):

| Platform | Architecture | Asset | Typical hardware |
|----------|-------------|-------|------------------|
| Linux | amd64 | `ollama-mesh-linux-amd64` | Production GPU servers, x86 workstations |
| Linux | arm64 | `ollama-mesh-linux-arm64` | ARM servers, Graviton instances |
| macOS | Apple Silicon | `ollama-mesh-darwin-arm64` | Mac Studio, Mac Pro, M-series dev machines |
| macOS | Intel | `ollama-mesh-darwin-amd64` | Intel Macs |
| Windows | amd64 | `ollama-mesh-windows-amd64.exe` | Windows GPU workstations |
| Docker | multi-arch | `ghcr.io/anirudhx7/ollama-mesh` | Any container orchestrator |

> **macOS Gatekeeper:** binaries are not yet Apple-notarized. Clear the quarantine flag once: `xattr -d com.apple.quarantine ollama-mesh`.

All builds and `checksums.txt` on the [releases page](https://github.com/Anirudhx7/ollama-mesh/releases/latest).


**Build from source:**
```bash
git clone https://github.com/Anirudhx7/ollama-mesh
cd ollama-mesh
make build
./ollama-mesh
```

Point your LLM clients at `:11434`. ollama-mesh speaks the Ollama API and passes through Ollama's OpenAI-compatible `/v1` endpoints - both `ollama` clients and OpenAI SDKs work unchanged.

**Integration guides:** [Open WebUI](docs/integrations/open-webui.md) · [Continue](docs/integrations/continue.md) · [LibreChat](docs/integrations/librechat.md) · [AWS EC2 deploy](docs/deploy/aws-ec2.md)

---

## Configuration

There is no config file. ollama-mesh is DB-first: everything lives in `mesh.db` (SQLite), and you configure it entirely through the admin dashboard or the REST API - nothing to hand-edit, nothing to redeploy for a settings change.

**First boot:**
```bash
./ollama-mesh              # or --db /path/to/mesh.db to pick the database location
```
The binary opens (or creates) `mesh.db`, starts blank-slate, and prints a banner pointing you at the dashboard. Log in at `http://localhost:8080` with `admin` / `admin` - you'll be forced to set a new password on first login.

**Secrets at rest:** cloud provider API keys, mesh-issued API keys, the LiteLLM key, HuggingFace token, and webhook secret are encrypted in `mesh.db` with AES-256-GCM. The encryption key lives in `mesh.db.key`, generated next to the database on first boot (0600 permissions) - back it up alongside `mesh.db`, since losing it means re-entering those secrets. To supply your own key instead (e.g. from a secrets manager), set `MESH_ENCRYPTION_KEY` to a base64-encoded 32-byte value before starting the binary; `mesh.db.key` is not created when this is set. Upgrading from an older version that stored these fields as plaintext encrypts them automatically on first boot - no manual migration step.

From there, everything is a dashboard page or an `/admin/v1/...` API call:

| Area | Where |
|---|---|
| GPU nodes | **GPU Nodes** page, or `install.sh`'s network-discovery wizard (`--seed-node` under the hood) |
| API keys, rate limits, model allow-lists, quotas | **API Keys** page |
| Routing strategy, timeouts, retries, session affinity, queueing, thermal watchdog | **Settings → Advanced Routing** |
| Cloud overflow providers (OpenAI/Anthropic), cost-per-1k, spend caps | **Settings → Cloud Providers** / **Cloud Spend Cap** |
| Docker auto-discovery, HA peer monitoring, webhooks | **Settings** (dedicated cards for each) |
| Model warmup schedule | **Settings → Global Warmup**, or per-node in the **Warmup** page |
| Model context windows | **Settings → Model Context Windows** |
| Proxy/admin ports, CORS, access log | **Settings → Proxy Configuration** / **Admin & Security** |

Prefer scripting it? Every one of those pages is a thin wrapper over `GET/PUT /admin/v1/settings`, `/admin/v1/nodes`, `/admin/v1/keys`, and `/admin/v1/cloud-providers` - GitOps-style operators can drive the same REST API from an init job instead of clicking through the UI.

---

## How Routing Works

```
Request arrives at :11434
    │
    ▼
Auth middleware: Bearer token validation → rate limit → quota check → model allow-list
    │
    ▼
Request queue: absorbs traffic spikes (configurable depth + timeout)
    │
    ▼
Router: extract model name from JSON body
    │
    ├── X-Session-ID present + session affinity enabled?
    │   └── Yes → route to pinned node (KV-cache affinity)
    │
    ├── Model warm in VRAM on any node?
    │   └── Yes → route to warm node with least active connections
    │
    ├── All nodes healthy?
    │   └── Yes → VRAM-fit placement (most free VRAM) or least-connections fallback
    │
    └── All nodes busy/down?
        └── Cloud overflow → OpenAI/Anthropic → response format-translated → cost logged
```

The router polls `/api/ps` on each node every 2 seconds. State is real-time, not cached guesses.

**Config hot-reload:** `kill -HUP <pid>` or `POST /admin/v1/config/reload` re-syncs live routing/nodes/keys/cloud-providers from `mesh.db` without dropping connections. (Listen ports/addresses and a few other startup-only settings still need a restart - the dashboard flags which ones.)

---

## Model Warmup

ollama-mesh proactively keeps priority models loaded in VRAM between requests. Without this, idle models get evicted and the next request pays the cold-start tax.

Configure it in the dashboard's **Settings → Global Warmup** card: enable it, set the interval (default every 5 minutes), and list your highest-traffic models. Per-node warmup overrides live on the **Warmup** page.

---

## Cloud Fallback Setup

Cloud providers are used **only** when all local inference nodes (Ollama, vLLM, TGI, llama.cpp, MLX) are unavailable or at capacity. Local GPU always wins. Set `enabled: true` on any provider:

```yaml
cloud_providers:
  - name: openai-overflow
    provider: openai
    base_url: https://api.openai.com
    api_key: sk-...
    default_model: gpt-4o-mini
    cost_per_1k_tokens: 0.00015
    enabled: true
```

Ollama-native (`/api/*`) requests that fall back to cloud get the OpenAI response translated back to Ollama NDJSON - clients never see a format difference.

---

## Operational Topology

| Port | Service | Auth |
|------|---------|------|
| `:11434` | Ollama-compatible endpoint - drop-in replacement | Per-key Bearer token |
| `:8080` | Admin dashboard + REST API | Admin token |
| `:9090` | Prometheus metrics | Unauthenticated (scrape target) |

---

## Admin API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/v1/nodes` | Node list: status, VRAM usage, active models, source labels |
| GET | `/admin/v1/keys` | API keys: usage stats, monthly cost, token totals |
| GET | `/admin/v1/metrics/savings` | Cost savings vs pure cloud - current process lifetime |
| GET | `/admin/v1/cloud/providers` | Cloud fallback providers: status, spend |
| GET | `/health` | 200 OK when control plane is ready (unauthenticated, for LB health checks) |
| POST | `/admin/nodes/{name}/drain` | Drain node for maintenance |
| PATCH | `/admin/keys/{name}` | Mutate key rate limits, quotas, model allow-lists at runtime |
| PATCH | `/admin/nodes/{name}` | Override `vram_total_mb`, `gpu_model` at runtime |
| POST | `/admin/v1/config/reload` | Hot-reload config without SIGHUP |

---

## Observability Stack

### Prometheus

14 metrics exported at `:9090/metrics`:

- `ollamamesh_requests_total` - total proxied requests (labels: key, model, node, status)
- `ollamamesh_request_duration_seconds` - histogram of request latency
- `ollamamesh_active_connections` - active connections per node
- `ollamamesh_node_healthy` - health gauge per node (1=healthy, 0=unhealthy)
- `ollamamesh_cache_hits_total` - warm-model cache hits
- `ollamamesh_cache_misses_total` - cold-start cache misses
- `ollamamesh_tokens_total` - tokens processed (labels: key, node)
- `ollamamesh_retries_total` - upstream failover retries per node
- `ollamamesh_cloud_fallbacks_total` - cloud overflow events per provider
- `ollamamesh_quota_rejections_total` - 429 quota enforcement events (labels: key, period)
- `ollamamesh_panics_total` - recovered handler panics
- `ollamamesh_queue_depth` - current request queue depth
- `ollamamesh_queue_timeouts_total` - queued requests that timed out before getting a node
- `ollamamesh_warmup_pings_total` - proactive keepalive pings per model/node

### Grafana

Import `grafana/ollama-mesh.json` into Grafana. Point the Prometheus datasource at `:9090`. Pre-built panels: VRAM utilization, request throughput, latency percentiles, cloud fallback rate, cost attribution.

### Structured Logging

`--log-format json` emits slog JSON objects that Loki, Datadog, Fluentd, and Splunk parse natively. Every request logged with: key name (never the key value), model, target node, HTTP status, latency, request ID.

---

## Competitive Positioning

| | ollama-mesh | LiteLLM | nginx/HAProxy | Portkey/Helicone |
|---|---|---|---|---|
| **GPU-aware routing** | ✅ Polls VRAM state every 2s | ❌ Treats Ollama as a dumb URL | ❌ No GPU visibility | ❌ Cloud-only |
| **Warm-model routing** | ✅ Routes to node with model in VRAM | ❌ | ❌ | ❌ |
| **VRAM-fit placement** | ✅ Cold requests → most free VRAM | ❌ | ❌ | ❌ |
| **KV-cache session affinity** | ✅ X-Session-ID sticky routing | ❌ | ❌ | ❌ |
| **Cloud overflow (consent-first)** | ✅ Off by default, explicit opt-in | ✅ (default on) | ❌ | ✅ (cloud-native) |
| **Savings tracking** | ✅ Real parsed token math | ❌ | ❌ | Partial |
| **Per-key cost attribution** | ✅ Tokens + USD per key per month | ✅ | ❌ | ✅ |
| **Single binary, zero deps** | ✅ Go static binary | ❌ Python + deps | ✅ | ❌ SaaS |
| **Embedded dashboard** | ✅ React UI in the binary | Separate UI | ❌ | SaaS dashboard |
| **Prometheus + Grafana** | ✅ 14 metrics + included dashboard | ✅ | Partial | ❌ |
| **Local-first architecture** | ✅ GPU traffic never leaves your network | ❌ Cloud-centric | ✅ | ❌ |

### Use ollama-mesh when:

- You have on-premises GPU hardware running Ollama, vLLM, TGI, llama.cpp, or MLX (Apple Silicon) and want to maximize utilization before paying for cloud tokens.
- You need per-key auth, rate limiting, cost attribution, and a usage dashboard without standing up a Python service.
- You need GPU-warm-first routing to eliminate cold-start latency in multi-agent workflows.
- You want cloud overflow that is explicitly opt-in - not a default that silently generates bills.
- You need a single static binary that ops teams can deploy and manage like any other Go service.

### Use LiteLLM instead when:

- You route primarily between cloud providers (Bedrock, Vertex, Cohere) and don't have on-premises GPU hardware.
- You are already invested in the LiteLLM ecosystem and don't need GPU-aware routing.
- You are comfortable with the Python operational footprint.

---

## Roadmap

See [ROADMAP.md](ROADMAP.md) for the full open-core strategy.

---

## Documentation

- [Production Deployment Guide](docs/PRODUCTION.md)
- [Savings Math](docs/SAVINGS-MATH.md) - how every dollar figure is computed
- [Use Cases](docs/USE-CASES.md)
- [Security](SECURITY.md)
- [Changelog](CHANGELOG.md)
- [Contributing](CONTRIBUTING.md)

---

## License

Apache-2.0 - see [LICENSE](LICENSE) and [NOTICE](NOTICE). The open-source core is free for any use, including commercial. Enterprise governance/compliance features are offered separately under a commercial license (see [ROADMAP.md](ROADMAP.md)).
