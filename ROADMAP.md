# ollama-mesh Roadmap

ollama-mesh is a **single-instance, self-hosted inference control plane**. This roadmap is
the evolution of one thing - the **router's intelligence** - not a march toward distributed
infrastructure. It runs as one static Go binary, SQLite only, in front of multiple GPU
backends (Ollama, vLLM, TGI, llama.cpp) behind a single OpenAI-compatible endpoint.

## The moat: router intelligence progression

```
Warm-aware router
      ↓
State-aware router
      ↓
KV-preserving router
      ↓
Predictive router
      ↓
Context-aware router
```

Everything below is in service of moving down that line. The open-source core is
production-hardened and free for any use under Apache-2.0.

---

## Shipped (open-source core, Apache-2.0)

**Routing & scheduling**
- Warm-aware routing - route to the node already holding the model warm in VRAM
- VRAM-aware cold placement - cold requests go to the node with the most free VRAM (v0.8.0)
- Weighted placement scoring - warm / free-VRAM / queue-depth / health / success factors, with model pinning and post-failure node cooldown (v0.11–v0.14)
- Request queue with backpressure - configurable depth + timeout (v0.8.2)
- Persistent warm state - the warm map survives restarts and is reconciled against live `/api/ps` on boot, so routing intelligence is not lost on restart (v0.11–v0.14)

**KV / session preservation**
- Session affinity - `X-Session-ID` sticky routing with TTL and graceful fallback, keeping backend KV cache warm across multi-turn conversations (v0.6.0)

**Prediction**
- Predictive prewarming - an in-memory transition ring buffer plus time-of-day patterns warm the next likely model *before* the request arrives (v0.11–v0.14)

**Multi-runtime**
- `RuntimeProbe` abstraction - Ollama, vLLM, TGI, and llama.cpp behind one OpenAI-compatible endpoint; path-aware routing (`/api/*` → Ollama, `/v1/*` → any) (v0.10.0)

**Reliability & operations**
- Pre-stream failover - dead node → retry alternate → cloud → 502 (v0.3.x)
- Active/active HA - run two independent instances behind any TCP/L4 load balancer; peer `/health` awareness eliminates the single-proxy SPOF (v0.7.0)
- Cost-aware cloud overflow - OpenAI/Anthropic fallback only when local capacity is saturated, with real per-token cost tracking and savings math (v0.2.x–v0.3.x)
- Auth, per-key model allow-lists, rate limits, and daily/monthly quotas persisted across restarts (v0.3.x)
- Observability - embedded admin dashboard, 14 Prometheus metrics, append-only audit log, analytics (v0.2.x–v0.9.x)
- Day-2 ops - node drain, runtime key/node mutation, SIGHUP + HTTP config reload, structured JSON logging (v0.9.0)
- `ollama-mesh bench` - reproducible cold-vs-warm TTFT measured through the mesh proxy (v0.11–v0.14)
- Router decomposition - `placement` / `health` / `queue` split behind interfaces, so the next stages extend safely without touching the hot path (v0.11–v0.14)

Full, dated release history lives in [CHANGELOG.md](CHANGELOG.md).

---

## Now - Validation on real hardware

The next milestone is proof, not features. The current README benchmark was measured on a
node where the model did not fully fit in VRAM (documented honestly there). Before the
router intelligence deepens further, it gets validated on dedicated hardware:

- Cold-vs-warm TTFT on NVIDIA GPUs where the model fully fits in VRAM (n ≥ 10 per scenario,
  direct-to-node control run, `--json` output, hardware documented)
- Warm-aware routing vs round-robin across 2+ nodes - the number no plain load balancer can produce
- Heterogeneous fleet validation: NVIDIA + Apple Silicon backends behind one endpoint
- Published, reproducible results via the [`bench/`](bench/) harness

Community feedback from early deployments shapes the priority of everything below.

---

## Next

### Step 6 - Prefix locality hints
Route requests that share a large stable prefix (system prompt + retrieved documents) to
the node that last served that prefix, so backend KV cache is reused **across users**, not
just within a single session.

- Hash the stable prefix (system prompt / RAG context); ignore the final user message
- Store `prefix_hash → last_node` with a TTL
- Soft-prefer the last node; never hard-lock - reroute if it is unhealthy, cold, or full

**Payoff:** RAG pipelines, shared corporate knowledge bases, contracts/legal - anywhere many
requests share a large prefix. This is the deepest KV-preservation stage of the moat.

---

## Later - under consideration (all within the single-instance model)

Ordered by pull, not promise - items graduate to "Next" when real deployments ask for them:

- **Scheduler depth** - extend placement scoring with queue-time and model-load-cost
  awareness (estimated total latency per candidate node, not just static weights)
- **Predictive accuracy hardening** - measure prewarming hit-rate and tune against real
  traffic patterns rather than adding new prediction signals
- **Deeper GPU observability** - per-model tokens/sec, cold-start counters, warm-hit
  ratios surfaced as first-class dashboard and Prometheus signals
- **SQLite write-path hardening at 20+ nodes** - the single-gateway design lives or dies
  on write throughput; benchmark and tune before it becomes a ceiling
- **Additional runtime adapters** (TensorRT-LLM, SGLang, …) - strictly demand-driven;
  added when a real deployment needs one, not speculatively

---

## Explicitly NOT on the roadmap (by design)

ollama-mesh is single-instance by design. These are deliberately out of scope - they trade
the product's core simplicity for infrastructure it doesn't need:

- ❌ etcd or any external datastore (SQLite only)
- ❌ distributed / shared state between instances
- ❌ multi-router consensus HA, leader election, gossip protocols, CRDTs - HA is achieved by running independent instances behind an L4 load balancer
- ❌ federated / multi-region mesh
- ❌ direct KV transport or shared KV between nodes
- ❌ tenant isolation and compliance packs (HIPAA/SOC 2) - later-stage, not now
- ❌ prompt-based model selection / policy engine ("small prompts → 7B") - gateway territory
  already served well by LiteLLM; ollama-mesh routes *placement*, not *model choice*

---

## Design constraints

- **Single mesh instance; SQLite is the only datastore.**
- One static Go binary, zero runtime dependencies - no CGO, no Python, no Docker required.
- Multiple heterogeneous GPU backends behind one OpenAI-compatible endpoint.
- Every number on the dashboard is a real measurement - no fabricated data, ever.
- Built for infra/MLOps operators self-hosting AI on their own hardware.

---

## Commercial

The open-source core above - **including the routing intelligence that is the moat** - stays
free under Apache-2.0. A commercial offering (enterprise governance and a managed control
plane) is planned once the core moat is complete; its details are intentionally not part of
this public roadmap.
