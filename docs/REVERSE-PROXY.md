# vLLM Load Balancing: Why DIY nginx Breaks Down

If you're running more than one vLLM or Ollama node, you've probably already reached for nginx or HAProxy to put one address in front of them. This page covers what that setup actually looks like, where it starts to hurt, and how marbor replaces it with a single binary.

---

## What people build by hand

The typical DIY setup, in roughly this order:

1. **One `vllm serve` process per GPU or per model.** Each instance binds its own port and exposes an OpenAI-compatible `/v1` API. As soon as you have more than one, something needs to sit in front of them and pick which one handles each request.
2. **A reverse proxy in front of them.** nginx or Caddy terminating TLS, adding a basic-auth layer or an API-key check in a `map` block, and forwarding to whichever vLLM (or Ollama) process should handle the request.
3. **vLLM load balancing via an `upstream` block.** nginx or HAProxy is configured with an `upstream` listing every vLLM process, sometimes with a health check hitting `/health` or `/v1/models` - typically `round-robin` or `least_conn`, because that's what a generic load balancer knows how to do. This is the "vllm load balancer" / "vllm load balancing" setup people search for.
4. **Ollama external access, if Ollama nodes are in the mix too.** Ollama binds to `127.0.0.1:11434` by default; reaching it from another machine means `OLLAMA_HOST=0.0.0.0:11434` (or the systemd override equivalent) and opening the port on the firewall - there's no auth built into Ollama itself, so step 2's proxy layer has to cover it.
5. **Streaming proxy settings.** Both vLLM's and Ollama's chat/completions endpoints stream token-by-token. Getting this right through nginx means `proxy_buffering off`, `proxy_http_version 1.1`, `proxy_set_header Connection ''`, and usually a longer `proxy_read_timeout` than the nginx default - without these, streaming responses buffer, arrive in one chunk, or time out on long generations. These are the "ollama proxy settings" people search for and get wrong on the first pass, but the same settings apply in front of vLLM.

This is a real, working setup. It's also the point where the limits of a generic network load balancer start showing up.

---

## Where it breaks

**No warm-model awareness.** nginx and HAProxy route on connection count or round-robin - they have no idea which node currently has which model loaded. A request gets sent to whichever node is "next," even if that node just evicted the model to load something else, triggering a cold load when another node three seconds away already has it loaded. Factoring model-residency into routing is an application-layer decision, not something a TCP/HTTP-layer proxy can express.

**No session affinity.** A follow-up request in the same conversation can land on a different node than the first one, even though the first node still has the relevant context or KV cache warm. Generic load balancers can pin by cookie or source IP, but neither maps to "this session was already being served by the node that has the right model loaded."

**No VRAM-aware placement.** Deciding which node should load a new model - based on how much VRAM is free right now, not just which node answered a health check - isn't something an nginx `upstream` block can express. That decision has to happen above the proxy layer, and by hand it means someone tracking free VRAM per node in their head (or a spreadsheet) and manually assigning models to nodes.

**Manual failover, no auto-recovery.** If a node goes down, nginx marks it `down` after failed health checks and stops sending it traffic - that part works. What doesn't happen automatically: routing that request somewhere else *useful* (a warm node, or a cost-aware cloud fallback) rather than just failing over to whatever's next in the list, and there's no automatic recovery logic beyond "retry the health check." Getting real failover - cloud overflow, retry-on-different-node, or alerting - means writing and maintaining scripts around the proxy config.

**Deployment footprint.** Every additional node means editing the nginx config, reloading, and repeating for auth rules, rate limits, and metrics scraping - none of which nginx does for you out of the box for an LLM-shaped API. Auth is a manual `map` or basic-auth block, not per-key access control. Rate limiting is nginx's generic `limit_req`, which throttles by request rate, not by token usage or model. Visibility into who's using what model and how much it's costing requires bolting on separate log parsing.

None of this is a criticism of nginx or HAProxy - they do exactly what they're designed to do. The gap is that "route to the node with the right model already loaded, and enough VRAM for the next one" is a decision that needs application-level knowledge of vLLM/Ollama internals, which a generic reverse proxy was never built to have.

---

## What marbor replaces

marbor is a single static Go binary that sits where the nginx/HAProxy layer would - one address your clients point at, with the same OpenAI-compatible API surface vLLM and Ollama already expose (see [Integrations](INTEGRATIONS.md)) - but with routing logic built for exactly this problem:

- **Warm-first routing.** marbor polls each node to see what's currently loaded - vLLM's own `/v1/models` on vLLM nodes, Ollama's own `/api/ps` on Ollama nodes - and routes each request to a node that already has the requested model loaded, instead of round-robin.
- **VRAM-aware placement.** When a model isn't loaded anywhere, marbor places it based on which node currently has room. This uses real GPU telemetry where the optional marbor agent is installed; without the agent, it falls back to operator-declared VRAM (see "Honest limitations" - this fallback path is the normal one for vLLM specifically, since vLLM's own API doesn't report VRAM usage).
- **Cost-aware cloud overflow.** When no local node can serve a request, marbor can route it to a configured cloud provider instead of failing - see [Use Cases](USE-CASES.md) for how this fits together.
- **Per-key auth, rate limits, and a dashboard** replace the hand-rolled nginx `map`/basic-auth block and `limit_req` directive, with a live view of what's running where.
- **One binary, zero external dependencies**, instead of an nginx/HAProxy config plus whatever scripts have accumulated around it for health checks and failover.

You still run marbor in front of one or more vLLM or Ollama nodes the same way you'd run nginx - the difference is what happens inside the proxy layer once a request arrives.

---

## Honest limitations (current state)

- marbor does not replace the TLS-termination or general-purpose reverse-proxy role nginx/HAProxy/Caddy play for the rest of your infrastructure - it's built for the vLLM/Ollama routing decision specifically, not as a general web server. Running marbor behind your existing TLS-terminating proxy, rather than instead of it, is a normal deployment shape.
- Warm-model detection (which node has which model currently loaded) works live for both runtimes: Ollama via `/api/ps`, vLLM via `/v1/models`.
- VRAM/free-capacity is a separate signal from warm-model detection, and it isn't available the same way for both runtimes. vLLM's own API doesn't report VRAM usage at all, so VRAM-aware placement for vLLM nodes depends on the optional marbor agent (real `nvidia-smi`-derived telemetry) or an operator-declared `vram_total_mb` value - never an estimate. Ollama nodes have the same dependency for remote GPU telemetry; the local node's GPU metrics come from `nvidia-smi` on the marbor host directly regardless of runtime.
- High availability is "run marbor behind your own TCP/L4 load balancer" - marbor itself is a single-instance control plane, not a distributed cluster. This is a deliberate trade-off, not a gap expected to close.
