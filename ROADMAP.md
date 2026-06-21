# ollama-mesh Roadmap

## Open-Core Strategy

ollama-mesh follows an open-core model. The open-source core delivers production-grade GPU-aware routing, observability, and cost tracking. The commercial enterprise tier extends this with multi-runtime support, enterprise compliance, high-availability topology, and multi-tenant resource governance.

---

## Open-Source Core (MIT License)

The foundation that ships today. Production-hardened, fully functional, zero commercial gatekeeping.

### Phase 1 — Trustworthy Foundation ✅

Goal: Every number on the dashboard is a real measurement. No fake data anywhere.

- [x] Auth token validation — constant-time exact match, not substring
- [x] Port derivation — URL parsing, not array index arithmetic
- [x] VRAM calculation — real Ollama `/api/ps` data, not synthetic arithmetic
- [x] GPU telemetry — real nvidia-smi data or explicit null; no random numbers
- [x] Mutex-protected auth rate limit maps — eliminated race condition
- [x] API key timestamps — real creation time, not hardcoded date
- [x] UI auth — proper token prompt; no silent fallback
- [x] nvidia-smi integration — shell exec, XML parse, VRAM total/used, temperature, power draw
- [x] Router tests — warm-first logic, model loaded vs not loaded
- [x] Integration tests — mock Ollama HTTP server, routing decision verification
- [x] Streaming tests — unbuffered delivery, token tracking, mid-stream node death, SSE passthrough
- [x] Zero-config first run — auto-detects localhost:11434, generates API keys

### Phase 2 — Hybrid Routing ✅

Goal: Cloud fallback with full financial transparency. The feature no competitor has.

- [x] Cloud provider config — OpenAI and Anthropic endpoints + keys in config.yaml
- [x] Router fallback logic — local nodes exhausted → cloud overflow (consent-first, off by default)
- [x] Cost tracking — real parsed token cost per request (local = $0, cloud = provider rate)
- [x] Savings dashboard — "$X saved this month vs pure cloud" from real token math
- [x] Docker auto-discovery — scans Docker socket for `ollama/ollama` containers, polls every 30s
- [x] Grafana dashboard — included JSON, one-click import
- [x] Admin API versioned at `/admin/v1/`

### v0.2.x — Dashboard and Observability ✅

Goal: Full operational visibility into the mesh.

- [x] Analytics page — 24-hour area chart (local vs cloud), savings stats, per-model breakdown
- [x] Model catalog page — cross-node VRAM view, warm status badges, search
- [x] Request log page — live feed, filter by key/model/status, status indicators
- [x] Rate limit headers — `X-RateLimit-Limit/Remaining/Reset` on every response
- [x] Webhook notifications — `node_down`/`node_up` with HMAC-SHA256 signatures
- [x] Audit logging — append-only JSON-lines, crypto/rand request IDs
- [x] 11 Prometheus metrics on `:9090/metrics`
- [x] `GET /health` — unauthenticated, for load balancers
- [x] Real savings math — parsed token counts, null/"—" when unavailable
- [x] Mid-stream abort logging — recorded in metrics, admin log, audit
- [x] VRAM fit indicator — green/yellow/red badges per model per node
- [x] `make demo` — populated dashboard in <60s, no Ollama required
- [x] Tokens/sec column in live request log

### v0.3.x — Multi-Tenant Production ✅

Goal: Per-key isolation and enforcement for shared infrastructure.

- [x] Per-key model allow-lists — enforced at proxy, 403 on violation
- [x] Pre-stream retry/failover — dead node → retry alternate → cloud → 502
- [x] Upstream ResponseHeaderTimeout — hung node no longer blocks client/leaks goroutines
- [x] `GET /v1/models` — OpenAI-schema aggregated model list from healthy nodes
- [x] Per-key token totals + estimated cost attribution
- [x] Per-key hard quotas — `daily_limit`/`monthly_limit` (429 on breach), persisted across restarts
- [x] Cloud format translation — Ollama-native requests overflowing to cloud get OpenAI responses translated back to Ollama NDJSON

### v0.5.x — Cluster Telemetry ✅

Goal: Accurate VRAM visibility across the entire cluster.

- [x] Remote VRAM telemetry — `/api/ps` `size_vram` decoding fix, per-node used-VRAM summed cluster-wide
- [x] `vramSource` labels — nvidia/api/declared/none provenance on every figure
- [x] Operator-declared `vram_total_mb` for remote nodes without nvidia-smi access

### v0.8.x — Operational Excellence ✅

Goal: Production-grade operational controls.

- [x] Proactive model warmup — `keep_alive` pings on configurable schedule
- [x] SIGHUP hot-reload — re-read config without restarting
- [x] Request queue with backpressure — configurable `queue_max_depth` + `queue_timeout_ms`

### v0.9.x — Enterprise Operations ✅

Goal: Day-2 operational workflow support.

- [x] Node drain — `POST /admin/nodes/{name}/drain` for zero-downtime GPU maintenance
- [x] Runtime key mutation — `PATCH /admin/keys/{name}` without key rotation or restart
- [x] Runtime node mutation — `PATCH /admin/nodes/{name}` for `vram_total_mb`, `gpu_model` overrides
- [x] Config hot-reload via HTTP — `POST /admin/v1/config/reload` for environments without SIGHUP
- [x] Structured JSON logging — `--log-format json` for log aggregators
- [x] Mobile-responsive dashboard
- [x] Warm-vs-cold TTFT benchmark harness (`bench/`)
- [x] Security regression test gate
- [x] KV-cache / context affinity — `X-Session-ID` sticky routing with TTL
- [x] HA peer awareness — active/active, each instance polls peers' `/health`
- [x] VRAM-aware placement — cold requests route to node with most free VRAM
- [x] Model Advisor page — model catalog with VRAM fit per node

### Open-Source Backlog

- [ ] SQLite analytics persistence — counters survive restarts; deferred until retention semantics defined
- [ ] Remote node GPU telemetry — sidecar agent for nvidia-smi on non-mesh nodes (deferred until demand validated)
- [ ] Per-model reference rates — separate cloud-equivalent rates per model for more accurate savings math
- [ ] Input/output token split pricing — match cloud pricing structure (prompt vs completion rates)

---

## Commercial Enterprise Tier (Closed Source)

The monetization engine. Capabilities that enterprise procurement requires and open-source economics cannot sustain.

### Tier 1 — Backend Plurality

**Problem:** Enterprise GPU fleets run more than Ollama. Production ML teams deploy vLLM for throughput, TGI for Hugging Face ecosystem compatibility, and TensorRT-LLM for NVIDIA-optimized inference. A routing proxy locked to a single runtime is a single point of vendor dependency.

**Solution:** Native routing adapters for each inference runtime, sharing the same warm-model awareness, health polling, and VRAM-fit logic.

| Runtime | Health Probe | VRAM Discovery | Warm-Model Detection | Routing Strategy |
|---------|-------------|----------------|---------------------|------------------|
| Ollama | `/api/ps` | `size_vram` field | Model list from `/api/ps` | Warm-first → least-connections |
| vLLM | `/health` + `/v1/models` | PagedAttention KV-cache metrics | Running model from `/v1/models` | Warm-first → KV-cache utilization |
| Hugging Face TGI | `/health` | CUDA memory allocation | Model loaded state | Warm-first → queue depth |
| NVIDIA TensorRT-LLM | Triton health API | GPU memory from Triton metrics | Engine loaded state | Warm-first → batch utilization |

Each adapter implements the same `RuntimeProbe` interface — the router is runtime-agnostic. A single ollama-mesh instance can route across a heterogeneous fleet of Ollama, vLLM, and TGI nodes simultaneously.

### Tier 2 — Enterprise Compliance & Access Control

**Problem:** Shared API keys with flat rate limits do not satisfy enterprise security requirements. SOC 2, HIPAA, and ISO 27001 audits require identity-bound access, role-based permissions, and immutable audit trails.

**Solution:**

- **SSO Integration** — OIDC and SAML 2.0 connectors for Okta, Azure AD, Google Workspace, and any compliant IdP. Bearer tokens are exchanged for identity-bound session tokens. Every request is attributed to a named user, not a shared key.
- **Role-Based Access Control (RBAC)** — Granular permission model:
  - `admin` — full dashboard access, key management, node drain, config reload
  - `operator` — read-only dashboard, node status, no key management
  - `consumer` — inference-only, scoped to assigned models and quotas
  - Custom roles with per-endpoint permission grants
- **Immutable Audit Logs** — Cryptographically chained audit records (hash-linked entries). Tamper-evident. Export to SIEM (Splunk, Elastic, Datadog) via syslog or webhook. Satisfies SOC 2 Type II audit trail requirements.
- **Active Directory / LDAP Integration** — Group-to-role mapping. `CN=ml-engineers` → `operator` role. Automatic provisioning and de-provisioning on directory sync.

### Tier 3 — High-Availability Topology

**Problem:** A single proxy instance is a single point of failure. Enterprise SLAs (99.9%+) require the control plane to survive node failures, network partitions, and rolling upgrades without dropping requests.

**Solution:**

- **Stateless Proxy Architecture** — All routing state (node health, warm-model maps, affinity tables) synchronized across instances via an embedded distributed state layer. Any instance can serve any request. No leader election. No split-brain.
- **Cross-Node State Sync** — Lightweight gossip protocol (memberlist) for cluster membership. Routing decisions are eventually consistent within one poll interval (2s). Quota counters synchronized via CRDTs — monotonic, conflict-free across partitions.
- **Zero-Downtime Upgrades** — Rolling restart with connection draining. The old instance completes in-flight streams while the new instance accepts new connections. No 502s during upgrade.
- **Health-Aware Load Balancing** — Each proxy instance exposes `/health` with degradation signals. Upstream TCP load balancers (HAProxy, AWS NLB, Envoy) can route around unhealthy instances automatically.

### Tier 4 — Multi-Tenant Resource Governance

**Problem:** Shared GPU infrastructure without resource isolation leads to noisy-neighbor problems. One team's batch embedding job starves another team's latency-sensitive copilot. Finance needs per-department cost attribution.

**Solution:**

- **Department-Scoped Quotas** — Hierarchical quota model: `organization → department → team → key`. GPU compute windows (max concurrent requests, max tokens/hour) enforced at each level. Quotas cascade — a department exhausting its allocation does not affect other departments.
- **Priority Classes** — `critical` (copilot, customer-facing), `standard` (internal tools), `batch` (embeddings, overnight jobs). The request queue drains `critical` first. `batch` requests yield to `standard` under contention.
- **Cost Allocation Reports** — Monthly CSV/JSON export: tokens consumed, estimated cloud-equivalent cost, actual cloud spend, savings attributed — grouped by department, team, key, model. Finance-ready. Integrates with internal chargeback systems.
- **GPU Time Budgets** — Per-tenant maximum GPU-seconds per billing period. Prevents runaway batch jobs from monopolizing shared hardware. Configurable burst allowance for traffic spikes.

### Tier 5 — Managed Cloud Control Plane

**Problem:** Self-hosted control planes require operational investment — upgrades, backups, monitoring of the monitor. Some teams want the routing intelligence without the operational burden.

**Solution:**

- **Hosted Control Plane** — Multi-tenant SaaS. GPU nodes remain on-premises (inference traffic never leaves the customer's network). The control plane handles configuration, dashboards, alerting, and billing.
- **Metered Token Billing** — Pay-per-token for cloud overflow. Transparent pricing. No minimum commitment.
- **Fleet Management** — Centralized management of multiple ollama-mesh clusters across regions and environments. Unified dashboard, unified audit trail.

---

## Pricing Model

| Tier | Target | Pricing |
|------|--------|---------|
| **Open-Source Core** | Individual developers, small teams, homelabs | Free (MIT) |
| **Enterprise Self-Hosted** | Platform teams at 50–500 person companies | $500–$2,000/mo per cluster |
| **Enterprise Managed** | Teams that want routing intelligence without operational burden | Metered per-token + base fee |

---

## Release History

| Version | Date | Milestone |
|---------|------|-----------|
| v0.2.0 | 2026-06-11 | First release: 5 platform binaries, ghcr image |
| v0.2.1 | 2026-06-12 | Zero-config, make demo, real savings math |
| v0.3.0 | 2026-06-13 | Multi-tenant: allow-lists, quotas, failover, format translation |
| v0.5.0 | 2026-06-15 | Cluster VRAM telemetry, vramSource labels |
| v0.8.0 | 2026-06-17 | Model warmup, SIGHUP, request queue |
| v0.9.0 | 2026-06-21 | Node drain, runtime mutation, structured logging, HA, KV-cache affinity |
