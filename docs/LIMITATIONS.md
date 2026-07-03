# Known Limitations

This page documents what ollama-mesh does not do, what has been tested, and what to plan around in production. Infrastructure engineers evaluating the project for production use should read this before deploying.

---

## Deployment Topology

### What has been tested
The validated topology is **one ollama-mesh process on a single host, routing to one or more remote Ollama nodes**. This includes bare-metal and EC2 deployments. Multi-node routing, failover, and cloud overflow have been exercised in this configuration.

No distributed or multi-instance ollama-mesh topology has been tested. There is no coordination layer, no distributed lock, and no leader election. Running two instances of ollama-mesh pointing at the same config is not supported.

### No high availability or multi-region failover
ollama-mesh is a single process. If the host running it goes down, inference traffic stops until the process restarts. There is no hot standby, no floating IP handoff, and no automatic failover between mesh instances.

For production HA, the standard approach is to put a layer-4 load balancer (e.g., an AWS NLB) in front of two independent ollama-mesh instances, each with their own config, and accept that in-flight requests to the failed instance are lost. This works because ollama-mesh is stateless for the routing path — only the admin session is lost on restart.

---

## Docker Node Auto-Discovery
Docker-based auto-discovery works by scanning the Docker socket for containers running Ollama and registering them as nodes. Discovered nodes use the container's own network IP (from the Docker API's `NetworkSettings`) when one is available, which is correct for containers on a bridge network regardless of whether ollama-mesh itself runs on bare metal or inside another container on the same Docker network.

**Fallback to `127.0.0.1`** still applies when a container has no network IP to report — the `--network host` case, where the container shares the host's network namespace and has no private IP of its own. In that case the discovered address is only reachable if ollama-mesh is also running with host networking (or directly on bare metal).

Workarounds if a discovered node is unreachable:
- Run ollama-mesh on the host directly (bare-metal or VM), not inside a container.
- Run ollama-mesh in Docker with `network_mode: host`, matching the discovered node's networking mode.
- Disable auto-discovery and configure nodes manually in `config.yaml` using the correct container IP or hostname.

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

### Admin login lockout
The admin login endpoint throttles failed attempts per client IP: 5 failures within a 5-minute window trigger a 15-minute lockout (`429 Too Many Requests`, with a generic error that never reveals whether the username exists). A successful login clears the failure count for that IP. This state is in-memory and resets on process restart — an acceptable tradeoff since a meaningful brute-force run takes far longer than a typical restart cycle, and rotating credentials (not restarting the process) is the right response to a suspected compromise.

For defense in depth, still put a reverse proxy in front of the admin port (`8080`) and apply rate limiting there — nginx's `limit_req` directive or Cloudflare's rate limiting rules both work. The admin port should not be exposed to the public internet directly regardless.

Rate limiting on the proxy port (`11434`) is implemented per API key via a token bucket, separately from the admin login throttle above.

### Demo-mode auth bypass exists in the binary, but is not reachable in a real deployment
`admin.Server` has a `demoMode` flag that, when set, accepts a static `demo-session` bearer token in place of a real DB-backed session. It is set only by test code (`SetDemoMode` / `AdminToken`, called exclusively from `_test.go` files) — no CLI flag, config field, or environment variable in the shipped binary or `main.go` ever enables it, so a normally built and run `ollama-mesh` process has no code path that turns it on. (The public `/demo/` dashboard on the website is unrelated: it's a pure frontend flag, `VITE_FORCE_DEMO`, that makes the React app render entirely client-side mocked data — it never talks to a real `admin.Server` and never uses this token.) The flag stays in the shipped binary as dead code rather than being compiled out behind a build tag, since doing so cleanly requires reworking the ~20 test call sites that use it to authenticate against the real DB-backed session path instead.

---

## Data Persistence

### Analytics dashboard: traffic history restored, per-model breakdown still gaps after restart
Hourly traffic buckets (requests, tokens, local/cloud split, cost) are persisted to SQLite and now restored into the in-memory analytics store on startup, so the traffic charts show continuous history immediately after a restart instead of a dip.

The "By Model" table's Local/Cloud breakdown is not backfilled the same way. Per-model stats are persisted as an aggregate request count with no local/cloud split, but the dashboard needs that split — attributing the aggregate to either side on restart would fabricate a false 100%-local or 100%-cloud number, which violates the project's no-fake-data rule. So the by-model view still shows a gap that fills back in as new traffic arrives. No data is lost in either case — the SQLite records are intact.

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
