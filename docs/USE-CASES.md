# Who is ollama-mesh for?

The ops layer Ollama doesn't ship: auth, load balancing, cloud failover, and a cost dashboard - in one Go binary.

You point your apps at ollama-mesh instead of Ollama directly. Everything else stays the same: it speaks the Ollama API (and passes through Ollama's OpenAI-compatible `/v1` endpoints), so existing clients work unchanged.

---

## The three problems it solves

### 1. "Ollama is busy" kills your app

Ollama has no queue management and no failover. When your GPU is saturated or the box is down, requests fail or hang. ollama-mesh overflows those requests to OpenAI or Anthropic automatically - your client never sees an error, and you only pay for cloud when local can't serve.

**Who feels this:** anyone running an app, agent, or team workload against a single Ollama box.

### 2. Cold starts waste 30 seconds per model load

With multiple Ollama nodes, a naive load balancer (nginx round-robin) sends requests to nodes that don't have the model loaded, triggering a cold load from disk every time. ollama-mesh polls `/api/ps` on every node and routes to the one that already has the model warm in VRAM.

**Who feels this:** homelabs and teams with 2+ GPU boxes serving more models than fit on one card.

### 3. No visibility, no control

Vanilla Ollama has no auth, no rate limits, no metrics, no request log. Anyone on the network can use your GPU, and you can't see who used what or what cloud fallback cost you. ollama-mesh adds per-key auth with rate limits and model allow-lists, a live dashboard, Prometheus metrics, a Grafana dashboard, webhooks, and audit logging.

**Who feels this:** the platform engineer told "make AI work for the whole team" with on-prem GPUs and an OpenAI bill to justify.

---

## Audience, in order of fit

1. **Self-hosters running one Ollama box + apps that hit it.** You get cloud overflow, API keys, and a dashboard. Start here - one node plus one cloud key is a complete setup.
2. **Platform engineers at 50-500 person companies.** On-prem GPUs, team access control, cost visibility, Prometheus/Grafana integration for the existing monitoring stack.
3. **Multi-GPU homelabs.** Warm-first routing across nodes is built exactly for you.

**Who should NOT use this:** a single user chatting with one Ollama box occasionally. You have no concurrency problem and no bill to cut - you don't need a proxy.

---

## "Isn't this just LiteLLM?"

Same category (LLM gateway), opposite center of gravity:

| | LiteLLM | ollama-mesh |
|---|---------|-------------|
| Core competency | 100+ cloud providers, unified API | Local Ollama nodes as first-class citizens |
| Local awareness | Ollama is just another URL - no idea what's loaded in VRAM | Polls `/api/ps` every 2s, routes to the node with the model already warm |
| Direction of fallback | Cloud → cloud (provider A fails, try B) | Local → cloud (free GPU first, paid API only when forced) |
| Runtime | Python; Redis + Postgres for full features | Single static Go binary, zero dependencies |
| Built for | Teams all-in on cloud APIs | Teams with local GPUs trying to cut cloud bills |

One sentence: **LiteLLM treats local as a dumb endpoint; ollama-mesh treats local as the preferred resource and cloud as the overflow valve.**

vs. nginx upstream config: no auth, no model awareness (cold-start roulette), no dashboard, no cost tracking.

vs. doing nothing: fine until the first time two requests collide or the box goes down mid-demo.

---

## Honest limitations (current state)

- GPU metrics (VRAM/temperature/power) come from `nvidia-smi` on the mesh host only. Remote node GPUs are not visible yet - per-node telemetry is on the roadmap. Warm-model detection works for all nodes regardless (it uses Ollama's own `/api/ps`).
- Analytics and the request log are in-memory and reset on restart. Prometheus metrics and the audit log file persist.
- Cost savings are computed from real token counts parsed from responses. When a response carries no token data, the dashboard shows "—", never an estimate.
