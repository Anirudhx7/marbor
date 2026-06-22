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
- [x] 14 Prometheus metrics on `:9090/metrics`
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

### v0.9.1 — Enterprise Auth MVP (next)

Goal: Teams sign in with their company SSO and get API keys without admins editing YAML.

**New deps (pure Go, zero CGO, static binary preserved):**
- `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` — OIDC login (~+1MB)
- `modernc.org/sqlite` — user DB (~+4MB)
- `github.com/wneessen/go-mail` — SMTP email delivery (~+0.2MB)
- Binary: ~16MB → ~21MB

**OIDC login flow (OSS — gate on `auth.oidc.issuer_url` being set):**
- [ ] `GET /auth/login` — redirect to OIDC provider
- [ ] `GET /auth/callback` — validate JWT, extract email/sub, create pending user in SQLite
- [ ] `GET /auth/logout` — clear session
- [ ] Existing API key auth unchanged when OIDC not configured

**User provisioning:**
- [ ] SQLite user schema — `id (sub)`, `email`, `name`, `status (pending/active/suspended)`, `key_name`, `approved_by`, timestamps
- [ ] Admin dashboard — "Pending Users" tab with approve/deny controls
- [ ] Approve → auto-generate API key (crypto/rand) → send email (SMTP) with key + endpoint URL + curl example
- [ ] Deny → send rejection email with optional reason
- [ ] `GET /admin/v1/users` — list all users by status
- [ ] `POST /admin/v1/users/{id}/approve` — approve + key generation + email delivery
- [ ] `POST /admin/v1/users/{id}/deny` — deny with optional message
- [ ] `DELETE /admin/v1/users/{id}` — revoke (key deactivated, user notified)
- [ ] Per-user usage visible in admin (keys tied to user identity)

**Config:**
- [ ] `auth.oidc` block — `issuer_url`, `client_id`, `client_secret`, `redirect_url`, `scopes`
- [ ] `email` block — `smtp_host`, `smtp_port`, `smtp_user`, `smtp_password`, `from`, `tls`
- [ ] `config.example.yaml` updated with both sections

**Not in this phase:**
- SCIM directory sync
- RBAC roles beyond admin/consumer (comes after first paying enterprise customer)

### v0.9.2 — Multi-Backend Adapters (next after 0.9.1)

Goal: Route across heterogeneous inference fleets — not just Ollama.

**Problem:** Enterprise GPU fleets run vLLM for throughput, TGI for HuggingFace ecosystem, llama.cpp for single-node efficiency. All expose OpenAI-compatible `/v1/` APIs but differ in health probes and model-list endpoints.

**Solution:** `RuntimeProbe` interface — health check, VRAM/model-list discovery, warm detection. Router stays runtime-agnostic.

| Runtime | Health Probe | Warm-Model Detection |
|---------|-------------|---------------------|
| Ollama | `/api/ps` | `size_vram` field |
| vLLM | `/health` + `/v1/models` | Running model from `/v1/models` |
| HF TGI | `/health` | Model loaded state |
| llama.cpp / LM Studio | `/health` | Single-model, always warm |

- [ ] `RuntimeProbe` interface in `internal/router`
- [ ] Ollama adapter (refactor existing poller to implement interface)
- [ ] vLLM adapter — poll `/health`, `/v1/models`; detect loaded model
- [ ] TGI adapter — poll `/health`; single-model detection
- [ ] llama.cpp / LM Studio adapter — OpenAI /v1/ passthrough, always-warm model
- [ ] Node config: `type: ollama|vllm|tgi|llamacpp` field (default: `ollama` for backward compat)
- [ ] `config.example.yaml` updated with multi-backend examples

### Open-Source Backlog

- [ ] SQLite analytics persistence — counters survive restarts; deferred until retention semantics defined
- [ ] Remote node GPU telemetry — sidecar agent for nvidia-smi on non-mesh nodes (deferred until demand validated)
- [ ] Per-model reference rates — separate cloud-equivalent rates per model for more accurate savings math
- [ ] Input/output token split pricing — match cloud pricing structure (prompt vs completion rates)

---

## Commercial Enterprise Tier (Closed Source)

The monetization engine. Capabilities that enterprise procurement requires and open-source economics cannot sustain.

> **Note on backend plurality:** vLLM, TGI, llama.cpp, and LM Studio adapters ship in OSS v0.9.2 (see Open-Source Backlog above). Commercial Tier 1 covers the runtimes that require specialized commercial access or enterprise-only integrations: NVIDIA TensorRT-LLM (Triton API), proprietary inference clusters.

### Tier 1 — Enterprise Inference Runtimes

**Problem:** Enterprise GPU fleets at the largest deployments run NVIDIA TensorRT-LLM on Triton Inference Server. Triton's health/model APIs differ fundamentally from the OpenAI-compatible surface exposed by Ollama/vLLM/TGI.

**Solution:** Triton Inference Server adapter with native gRPC health probes, per-engine VRAM metrics from NVML, and batch utilization-aware routing.

| Runtime | Health Probe | VRAM Discovery | Routing Strategy |
|---------|-------------|----------------|------------------|
| NVIDIA TensorRT-LLM | Triton gRPC health | GPU memory from NVML | Warm-first → batch utilization |
| Custom proprietary | Configurable HTTP probe | Operator-declared | Configurable strategy |

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

| Tier | Target | Pricing | Key Features |
|------|--------|---------|--------------|
| **OSS Core** | Individual developers, homelabs, SMBs | Free (MIT) | GPU-aware routing, cloud fallback, basic API keys, Prometheus metrics |
| **Team** | Platform teams at 10–500 person companies | $299/mo per cluster | OIDC/SSO, user provisioning, admin approval, per-user quotas, multi-backend |
| **Enterprise** | Regulated industries, large enterprises | $999–$2,000/mo per cluster | HA cluster, SOC2 audit logs, SCIM/LDAP, priority support, custom SLA |
| **Managed** | Teams without ops bandwidth | Metered per-token + base fee | Hosted control plane, GPU nodes stay on-prem |

> **Competitive context (June 2026):**
> - LiteLLM Enterprise Basic: $250/mo licensing + ~$1.9K/mo TCO (Python + Postgres + Redis + DevOps) = ~$2.2K/mo total
> - Bifrost: Apache 2.0 OSS (no commercial tier yet), 23+ providers, SSO/RBAC — but ZERO GPU/VRAM awareness
> - Our Team tier at $299/mo: undercuts LiteLLM's TCO by 7x, beats Bifrost on GPU intelligence, wins on ops simplicity (single Go binary, zero infrastructure)

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
| v0.9.1 | TBD | User registration, admin approval workflow, email API key delivery |
