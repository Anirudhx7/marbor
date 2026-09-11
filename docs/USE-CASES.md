# Ollama Alternatives: Who Is marbor For?

Looking for an alternative to Ollama once you outgrow a single GPU box? marbor is the infrastructure control plane neither Ollama nor a bare vLLM deployment ships: secure multi-tenant access, hardware-aware load balancing, cost-aware cloud overflow, and real-time GPU telemetry -- plus a marbor agent for remote telemetry, model operations, and node-side maintenance. vLLM+marbor is the primary stack this is built around; Ollama support is real and works the same way, not an afterthought.

You point your apps at marbor instead of your inference backend directly. Everything else stays the same: it exposes an OpenAI-compatible `/v1` API (and Ollama's own API too), so existing clients work unchanged.

Where the marbor agent is installed on remote GPU hosts, marbor can surface richer live telemetry, perform node-side operations, and support maintenance workflows without changing the client-side API. Where it isn't installed, the core router still works and falls back to the best available telemetry.

---

## Where marbor fits among Ollama alternatives

"Ollama alternative" covers a few different categories of tool, depending on what you actually need:

- **Desktop chat UIs** (LM Studio, GPT4All, Jan): a friendlier front end for running one model on one machine. Good fit for a single user on a laptop or workstation - not built for a fleet of GPU nodes or multiple concurrent users.
- **Bare inference servers** (vLLM, TGI, llama.cpp server): faster or more configurable serving of a single model on a single node than Ollama, but no cross-node routing, auth, or fleet visibility on their own.
- **API gateways** (LiteLLM, Bifrost, Portkey): sit in front of multiple providers/models and handle developer auth, unified schemas, and rate limits - but they don't do hardware-aware scheduling across your own GPU nodes.
- **marbor**: not a replacement for vLLM or Ollama themselves (it speaks both their APIs and runs alongside them on each node) and not a gateway - it's the control plane in between: warm-aware routing across N GPU nodes, VRAM-aware placement, cost-aware cloud overflow, and fleet operations, for the point where a single box or a hand-rolled nginx setup stops being enough.

If you're a single user on one box, a desktop UI or plain Ollama is the right call - see "Who should NOT use this" below. If you're past that point, keep reading.

---

## The three problems it solves

### 1. "The GPU is busy" kills your app

A bare vLLM or Ollama process has no queue management and no failover. When your GPU is saturated or the box is down, requests fail or hang. marbor routes those requests to a cost-aware cloud overflow target (like OpenAI or Anthropic) automatically -- your client never sees an error, and you only pay for cloud when local can't serve.

**Who feels this:** anyone running an app, agent, or team workload against a single vLLM or Ollama box.

### 2. Cold starts waste time on every model load

With multiple vLLM or Ollama nodes, a naive load balancer (nginx round-robin) sends requests to nodes that don't have the model loaded, triggering a cold load every time. marbor polls each node to see what's currently loaded and routes to the one that already has the model warm.

**Who feels this:** homelabs and teams with 2+ GPU boxes serving more models than fit on one card.

### 3. No visibility, no control

A bare vLLM or Ollama deployment has no auth, no rate limits, no metrics, no request log. Anyone on the network can use your GPU, and you can't see who used what or what cloud fallback cost you. marbor adds per-key auth with rate limits and model allow-lists, a live dashboard, Prometheus metrics, a Grafana dashboard, webhooks, audit logging, and a marbor agent for remote telemetry, operations, and maintenance.

**Who feels this:** the platform engineer told "make AI work for the whole team" with on-prem GPUs and an OpenAI bill to justify.

---

## Audience, in order of fit

1. **Self-hosters running one vLLM or Ollama box + apps that hit it.** You get cost-aware cloud overflow, API keys, and a dashboard. Start here - one node plus one cloud key is a complete setup.
2. **Platform engineers at 50-500 person companies.** On-prem GPUs, team access control, cost visibility, Prometheus/Grafana integration for the existing monitoring stack.
3. **Multi-GPU homelabs.** Warm-first routing across nodes is built exactly for you.

**Who should NOT use this:** a single user chatting with one box occasionally. You have no concurrency problem and no bill to cut - you don't need an orchestration or scheduling layer.

---

## The Architecture Trade-offs: marbor vs. Plain vLLM/Ollama + nginx

Platform teams often attempt to orchestrate local GPU nodes using generic network load balancers (like nginx or HAProxy). The table below details why a generic proxy falls short compared to a hardware-aware scheduling layer.

| Feature | Plain vLLM/Ollama + nginx | marbor |
|---|---|---|
| **Routing Intelligence** | Round-robin or least-connections (model-blind; causes constant cold-starts) | **Hardware-aware (routes to the node that already has the model warm in VRAM)** |
| **Failover Strategy** | Static failover or raw connection drop | **Cost-aware cloud overflow (retains 100% uptime with cloud fallback only when forced)** |
| **Availability** | Manual setup (complex nginx configurations and scripts) | **Single-instance control plane; run behind your own TCP load balancer for redundancy** |
| **Multi-Model VRAM Placement** | Handled manually per host | **Dynamic VRAM-fit placement based on active node capacity** |
| **Enterprise Controls** | No native auth, quotas, or rate limiting | **Per-key auth, token-bucket rate limits, and monthly hard quotas** |
| **Financial Visibility** | None (separate billing auditing required) | **Real-time savings dashboard tracked by actual token counts** |
| **Deployment Footprint** | External load balancers, configs, and scripts | **Single static Go binary; zero external dependencies** |

For a deeper look at exactly what breaks in a hand-rolled vLLM/Ollama + nginx setup - and what it actually takes to fix it yourself - see [vLLM/Ollama Reverse Proxy: The DIY Pain](REVERSE-PROXY.md).

---

## Where does LiteLLM fit?

LiteLLM is a popular and excellent gateway for provider abstraction, enterprise authentication, and user-level rate limiting.

Rather than a competitor, **marbor operates as a complementary layer beneath LiteLLM**.

When deployed together, LiteLLM manages developer access, unified schemas, and cloud provider API keys, while **marbor slots in directly below it as the physical scheduling and GPU orchestration layer**.

This stack offers the best of both worlds:
1. **LiteLLM (Gateway)**: Manages developer authentication, user quotas, and application-level routing.
2. **marbor (Scheduler)**: Interacts directly with local GPU nodes, tracking model warm-residency (vLLM's `/v1/models`, Ollama's `/api/ps`), managing queue depths, and executing cost-aware cloud overflow.

---

## Honest limitations (current state)

- GPU metrics (VRAM/temperature/power) for the local node come from `nvidia-smi` on the marbor host directly. Remote nodes get real GPU telemetry only if the optional marbor agent is installed on them (opt-in, not auto-deployed) - without it, remote GPU metrics fall back to operator-declared `vram_total_mb` or show "-". Warm-model detection (which node has which model currently loaded) works live for both runtimes regardless of agent installation - Ollama via its own `/api/ps`, vLLM via its own `/v1/models` (vLLM's API doesn't report VRAM usage, so VRAM-aware placement for vLLM nodes always depends on the agent or an operator-declared value).
- Analytics and the request log are in-memory and reset on restart. Prometheus metrics persist, and the audit log persists to the `audit_log` table in `marbor.db` with configurable retention.
- Cost savings are computed from real token counts parsed from responses. When a response carries no token data, the dashboard shows "-", never an estimate.
