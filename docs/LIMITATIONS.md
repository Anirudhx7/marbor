# Known Limitations

This page documents what ollama-mesh does not do, what has been tested, and what to plan around in production. Infrastructure engineers evaluating the project for production use should read this before deploying.

---

## Deployment Topology

### What has been tested
The validated topology is **one ollama-mesh process on a single host, routing to one or more remote Ollama nodes**. This includes bare-metal and EC2 deployments. Multi-node routing, failover, and cloud overflow have been exercised in this configuration.

No distributed or multi-instance ollama-mesh topology has been tested. There is no coordination layer, no distributed lock, and no leader election. Running two instances of ollama-mesh pointing at the same config is not supported.

### No high availability or multi-region failover
ollama-mesh is a single process. If the host running it goes down, inference traffic stops until the process restarts. There is no hot standby, no floating IP handoff, and no automatic failover between mesh instances.

For production HA, the standard approach is to put a layer-4 load balancer (e.g., an AWS NLB) in front of two independent ollama-mesh instances, each with their own config, and accept that in-flight requests to the failed instance are lost. This works because ollama-mesh is stateless for the routing path — only the admin session and in-memory request log are lost on restart.

---

## Docker Node Auto-Discovery
Docker-based auto-discovery works by scanning the Docker socket for containers running Ollama and registering them as nodes. Discovered nodes are always registered with the address `http://127.0.0.1:<port>`.

**This only works correctly on bare-metal or host-network deployments.** If ollama-mesh itself runs inside a Docker container without host networking, `127.0.0.1` resolves to the mesh container's own loopback, not the host. Discovered nodes will be unreachable.

Workarounds:
- Run ollama-mesh on the host directly (bare-metal or VM), not inside a container.
- Run ollama-mesh in Docker with `network_mode: host`.
- Disable auto-discovery and configure nodes manually in `config.yaml` using the correct container IP or hostname.

This is not a planned limitation — it is a consequence of how Docker networking works. Manual node configuration is always supported and is the recommended approach for non-host-network container deployments.

---

## GPU Telemetry

### VRAM usage
Per-node VRAM usage (how much VRAM each model is consuming) is fetched from each node's `/api/ps` endpoint. This is available for all nodes — local and remote — without any node agent.

### VRAM capacity
Total VRAM capacity is read from `nvidia-smi` on the host running ollama-mesh. For remote nodes, capacity must be declared explicitly via `vram_total_mb` in `config.yaml`. If neither is set, capacity is shown as `—` in the dashboard.

### Temperature and power draw
GPU temperature and power draw are only available for the node running on the same host as ollama-mesh, via `nvidia-smi`. Remote nodes do not report temperature or power draw.

This is by design. Getting telemetry from remote nodes would require a lightweight agent running on each GPU host. ollama-mesh deliberately avoids that dependency to stay a single static binary with no remote components to deploy or maintain.

If you need remote GPU temperature and power draw, options are: a Prometheus node exporter with the DCGM exporter on each GPU host, or a Grafana Agent with the nvidia_smi collector. These can be layered on top of ollama-mesh without any changes to the mesh itself.

---

## Admin Dashboard Security

### No account lockout on admin login
The admin login endpoint has no rate limiting or lockout. Brute-forcing the admin token is computationally infeasible (64 hex characters of random data), but the endpoint will accept an unlimited number of authentication attempts without throttling or temporary lockout.

For production, put a reverse proxy in front of the admin port (`8080`) and apply rate limiting there. nginx's `limit_req` directive or Cloudflare's rate limiting rules both work. The admin port should not be exposed to the public internet directly regardless.

Rate limiting on the proxy port (`11434`) is implemented per API key via a token bucket. This does not apply to the admin port.

---

## Data Persistence

### Request log is in-memory
The request log (last 50 requests visible in the dashboard) is held in memory only. Restarting ollama-mesh clears the request history. No request-level data is written to SQLite today. This is a known gap — SQLite persistence for the request log is planned but not yet implemented.

Prometheus metrics at `:9090` are the correct mechanism for durable request-level observability. Those metrics are scraped and stored by your Prometheus server independently of the ollama-mesh process lifecycle.

### Analytics dashboard shows a gap after restart
Hourly traffic buckets are persisted to SQLite. However, the in-memory analytics store is not pre-populated from SQLite on startup. After a restart, the analytics dashboard will show a dip covering the period between the last restart and when enough new traffic has accumulated to refill the in-memory view. No data is lost — the SQLite records are intact — but the dashboard display is temporarily incomplete.

### Routing rules persist to SQLite
Routing rules added via the admin API (the UI or `POST /admin/v1/routing/rules`) are written to SQLite and survive restarts. Rules defined in `config.yaml` are also loaded on startup. Both sources are merged at boot.

---

## Configuration Model
ollama-mesh uses a hybrid persistence model: `config.yaml` for static configuration and SQLite for runtime state. The boundary is not always intuitive:
- Settings changed via the admin UI are written back to `config.yaml`.
- Nodes added via the admin API are persisted to SQLite.
- Routing rules added via the admin API are persisted to SQLite and survive restarts.
- API keys and quota counters are persisted to the JSON state file (`auth.state_path`), not to `config.yaml` or SQLite.

If you manage ollama-mesh configuration via infrastructure-as-code (Ansible, Terraform, etc.), the safest approach is to treat `config.yaml` as the source of truth for all configuration and not rely on admin API mutations for anything that needs to survive a redeploy.

---

## Session Affinity
Session affinity is implemented and gated by the `routing.session_affinity` flag. When enabled, requests carrying an `X-Session-ID` header are pinned to the same backend node (within `routing.session_affinity_ttl`) so the node's KV-cache context stays warm across turns; the pin falls back to normal routing if that node becomes unhealthy or the TTL lapses. When disabled (the default), routing is stateless and the header is ignored.

---

## Out of Scope
The following are deliberate non-goals, not gaps to be filled:
- **TLS termination.** ollama-mesh does not handle TLS. Put nginx or a load balancer in front for HTTPS. This keeps the binary simple and puts TLS configuration where operators already manage it.
- **Remote GPU temperature and power draw.** Requires a node agent. Not built. Use DCGM exporter or nvidia_smi Prometheus collector on each GPU host.
- **Multi-instance coordination.** No distributed consensus, no Raft, no etcd dependency. Single-host deployment only.
- **Chat UI, model fine-tuning, or web scraping.** ollama-mesh is a proxy and router. These are out of scope.
- **Cloud provider breadth.** OpenAI and Anthropic are supported for cloud overflow. Supporting 100 providers (LiteLLM's approach) is not a goal.
