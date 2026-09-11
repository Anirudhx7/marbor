# Ollama Reverse Proxy: Why the DIY nginx Setup Breaks Down

If you're running more than one Ollama or vLLM node, you've probably already reached for nginx or HAProxy to put one address in front of them. This page covers what that setup actually looks like, where it starts to hurt, and how marbor replaces it with a single binary.

---

## What people build by hand

The typical DIY setup, in roughly this order:

1. **Ollama external access.** Ollama binds to `127.0.0.1:11434` by default. To reach it from another machine, you set `OLLAMA_HOST=0.0.0.0:11434` (or the systemd override equivalent) and open the port on the firewall. At this point the box is reachable by anything on the network - there's no auth built into Ollama itself.
2. **A reverse proxy in front of it.** Once more than one person or app needs access, a reverse proxy goes in front: nginx or Caddy terminating TLS, adding a basic-auth layer or an API-key check in a `map` block, and forwarding to the Ollama process.
3. **Ollama nginx reverse proxy settings for streaming.** Ollama's `/api/generate` and `/api/chat` responses stream token-by-token. Getting this right through nginx means `proxy_buffering off`, `proxy_http_version 1.1`, `proxy_set_header Connection ''`, and usually a longer `proxy_read_timeout` than the nginx default - without these, streaming responses buffer, arrive in one chunk, or time out on long generations. These are the "ollama proxy settings" people search for and get wrong on the first pass.
4. **More than one node.** Once a single GPU box runs out of VRAM for the models you need, a second (or third) node gets added, and nginx or HAProxy is configured with an `upstream` block listing both - typically `round-robin` or `least_conn`, because that's what a generic load balancer knows how to do.
5. **vLLM load balancing.** The same pattern repeats for vLLM: multiple `vllm serve` processes (often one per GPU or per model) behind the same kind of nginx/HAProxy upstream block, sometimes with a health check hitting `/health` or `/v1/models`.

This is a real, working setup. It's also the point where the limits of a generic network load balancer start showing up.

---

## Where it breaks

**No warm-model awareness.** nginx and HAProxy route on connection count or round-robin - they have no idea which node currently has which model loaded in VRAM. A request for a 30B model gets sent to whichever node is "next," even if that node just evicted the model to load something else. The result is a cold load from disk on a request that could have hit an already-warm node three seconds away. Neither nginx nor HAProxy can query `/api/ps` (Ollama's own warm-model endpoint) and factor it into routing, because that's an application-layer decision, not a TCP/HTTP-layer one.

**No session affinity.** A follow-up request in the same conversation can land on a different node than the first one, even though the first node still has the relevant context or KV cache warm. Generic load balancers can pin by cookie or source IP, but neither maps to "this session was already being served by the node that has the right model loaded."

**No VRAM-aware placement.** Deciding which node should load a new model - based on how much VRAM is free right now, not just which node answered a health check - isn't something an nginx `upstream` block can express. That decision has to happen above the proxy layer, and by hand it means someone tracking free VRAM per node in their head (or a spreadsheet) and manually assigning models to nodes.

**Manual failover, no auto-recovery.** If a node goes down, nginx marks it `down` after failed health checks and stops sending it traffic - that part works. What doesn't happen automatically: routing that request somewhere else *useful* (a warm node, or a cost-aware cloud fallback) rather than just failing over to whatever's next in the list, and there's no automatic recovery logic beyond "retry the health check." Getting real failover - cloud overflow, retry-on-different-node, or alerting - means writing and maintaining scripts around the proxy config.

**Deployment footprint.** Every additional node means editing the nginx config, reloading, and repeating for auth rules, rate limits, and metrics scraping - none of which nginx does for you out of the box for an LLM-shaped API. Auth is a manual `map` or basic-auth block, not per-key access control. Rate limiting is nginx's generic `limit_req`, which throttles by request rate, not by token usage or model. Visibility into who's using what model and how much it's costing requires bolting on separate log parsing.

None of this is a criticism of nginx or HAProxy - they do exactly what they're designed to do. The gap is that "route to the node with warm VRAM for this specific model" is a decision that needs application-level knowledge of Ollama/vLLM internals, which a generic reverse proxy was never built to have.

---

## What marbor replaces

marbor is a single static Go binary that sits where the nginx/HAProxy layer would - one address your clients point at, with the same Ollama-compatible and OpenAI-compatible API surface Ollama itself exposes (see [Integrations](INTEGRATIONS.md)) - but with routing logic built for exactly this problem:

- **Warm-first routing.** marbor polls `/api/ps` on every registered node and routes each request to a node that already has the requested model loaded, instead of round-robin.
- **VRAM-aware placement.** When a model isn't loaded anywhere, marbor places it based on which node currently has room, using real GPU telemetry where the optional marbor agent is installed, or operator-declared VRAM otherwise (see the "Honest limitations" note below).
- **Cost-aware cloud overflow.** When no local node can serve a request, marbor can route it to a configured cloud provider instead of failing - see [Use Cases](USE-CASES.md) for how this fits together.
- **Per-key auth, rate limits, and a dashboard** replace the hand-rolled nginx `map`/basic-auth block and `limit_req` directive, with a live view of what's running where.
- **One binary, zero external dependencies**, instead of an nginx/HAProxy config plus whatever scripts have accumulated around it for health checks and failover.

You still run marbor in front of one or more Ollama or vLLM nodes the same way you'd run nginx - the difference is what happens inside the proxy layer once a request arrives.

---

## Honest limitations (current state)

- marbor does not replace the TLS-termination or general-purpose reverse-proxy role nginx/HAProxy/Caddy play for the rest of your infrastructure - it's built for the Ollama/vLLM routing decision specifically, not as a general web server. Running marbor behind your existing TLS-terminating proxy, rather than instead of it, is a normal deployment shape.
- GPU metrics (VRAM/temperature/power) for the local node come from `nvidia-smi` on the marbor host directly. Remote nodes get real GPU telemetry only if the optional marbor agent is installed on them - without it, remote GPU metrics fall back to operator-declared `vram_total_mb` or show "-". Warm-model detection works for all nodes regardless, since it uses Ollama's own `/api/ps`.
- High availability is "run marbor behind your own TCP/L4 load balancer" - marbor itself is a single-instance control plane, not a distributed cluster (see the architecture laws in this project's operating rules). This is a deliberate trade-off, not a gap expected to close.
- vLLM support depends on the same warm-state signal Ollama exposes via `/api/ps`; where a vLLM deployment doesn't expose an equivalent, placement falls back to the same VRAM-declaration path used for nodes without the marbor agent installed.
