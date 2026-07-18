# Who is ollama-mesh for?

The infrastructure control plane Ollama doesn't ship: secure multi-tenant access, hardware-aware load balancing, cost-aware cloud overflow, and real-time GPU telemetry -- in one Go binary.

You point your apps at ollama-mesh instead of Ollama directly. Everything else stays the same: it speaks the Ollama API (and passes through Ollama's OpenAI-compatible `/v1` endpoints), so existing clients work unchanged.

---

## The three problems it solves

### 1. "Ollama is busy" kills your app

Ollama has no queue management and no failover. When your GPU is saturated or the box is down, requests fail or hang. ollama-mesh routes those requests to a cost-aware cloud overflow target (like OpenAI or Anthropic) automatically -- your client never sees an error, and you only pay for cloud when local can't serve.

**Who feels this:** anyone running an app, agent, or team workload against a single Ollama box.

### 2. Cold starts waste 30 seconds per model load

With multiple Ollama nodes, a naive load balancer (nginx round-robin) sends requests to nodes that don't have the model loaded, triggering a cold load from disk every time. ollama-mesh polls `/api/ps` on every node and routes to the one that already has the model warm in VRAM.

**Who feels this:** homelabs and teams with 2+ GPU boxes serving more models than fit on one card.

### 3. No visibility, no control

Vanilla Ollama has no auth, no rate limits, no metrics, no request log. Anyone on the network can use your GPU, and you can't see who used what or what cloud fallback cost you. ollama-mesh adds per-key auth with rate limits and model allow-lists, a live dashboard, Prometheus metrics, a Grafana dashboard, webhooks, and audit logging.

**Who feels this:** the platform engineer told "make AI work for the whole team" with on-prem GPUs and an OpenAI bill to justify.

---

## Audience, in order of fit

1. **Self-hosters running one Ollama box + apps that hit it.** You get cost-aware cloud overflow, API keys, and a dashboard. Start here - one node plus one cloud key is a complete setup.
2. **Platform engineers at 50-500 person companies.** On-prem GPUs, team access control, cost visibility, Prometheus/Grafana integration for the existing monitoring stack.
3. **Multi-GPU homelabs.** Warm-first routing across nodes is built exactly for you.

**Who should NOT use this:** a single user chatting with one Ollama box occasionally. You have no concurrency problem and no bill to cut - you don't need an orchestration or scheduling layer.

---

## The Architecture Trade-offs: ollama-mesh vs. Plain Ollama + nginx

Platform teams often attempt to orchestrate local GPU nodes using generic network load balancers (like nginx or HAProxy). The table below details why a generic proxy falls short compared to a hardware-aware scheduling layer.

| Feature | Plain Ollama + nginx | ollama-mesh |
|---|---|---|
| **Routing Intelligence** | Round-robin or least-connections (model-blind; causes constant cold-starts) | **Hardware-aware (routes to the node that already has the model warm in VRAM)** |
| **Failover Strategy** | Static failover or raw connection drop | **Cost-aware cloud overflow (retains 100% uptime with cloud fallback only when forced)** |
| **Availability** | Manual setup (complex nginx configurations and scripts) | **Single-instance control plane; run behind your own TCP load balancer for redundancy** |
| **Multi-Model VRAM Placement** | Handled manually per host | **Dynamic VRAM-fit placement based on active node capacity** |
| **Enterprise Controls** | No native auth, quotas, or rate limiting | **Per-key auth, token-bucket rate limits, and monthly hard quotas** |
| **Financial Visibility** | None (separate billing auditing required) | **Real-time savings dashboard tracked by actual token counts** |
| **Deployment Footprint** | External load balancers, configs, and scripts | **Single static Go binary; zero external dependencies** |

---

## Where does LiteLLM fit?

LiteLLM is a popular and excellent gateway for provider abstraction, enterprise authentication, and user-level rate limiting. 

Rather than a competitor, **ollama-mesh operates as a complementary layer beneath LiteLLM**. 

When deployed together, LiteLLM manages developer access, unified schemas, and cloud provider API keys, while **ollama-mesh slots in directly below it as the physical scheduling and GPU orchestration layer**. 

This stack offers the best of both worlds:
1. **LiteLLM (Gateway)**: Manages developer authentication, user quotas, and application-level routing.
2. **ollama-mesh (Scheduler)**: Interacts directly with local GPU nodes, tracking `/api/ps` model warm-residency, managing queue depths, and executing cost-aware cloud overflow.

---

## Honest limitations (current state)

- GPU metrics (VRAM/temperature/power) for the local node come from `nvidia-smi` on the mesh host directly. Remote nodes get real GPU telemetry only if the optional Node Agent is installed on them (opt-in, not auto-deployed) - without it, remote GPU metrics fall back to operator-declared `vram_total_mb` or show "-". Warm-model detection works for all nodes regardless (it uses Ollama's own `/api/ps`).
- Analytics and the request log are in-memory and reset on restart. Prometheus metrics persist, and the audit log persists to the `audit_log` table in `mesh.db` with configurable retention.
- Cost savings are computed from real token counts parsed from responses. When a response carries no token data, the dashboard shows "-", never an estimate.
