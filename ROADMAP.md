# ollama-mesh Roadmap

## Phase 1 - Trustworthy (complete)

Goal: Make the dashboard show real data. No fake numbers anywhere.

- [x] Fix auth token validation - exact match, not substring (security vulnerability)
- [x] Fix port derivation - URL parsing, not array index arithmetic
- [x] Fix VRAM calculation - real Ollama /api/ps data, not fake arithmetic
- [x] Remove random CPU/temperature metrics - real nvidia-smi data or null
- [x] Add mutex protection to auth rate limit maps - was a race condition
- [x] Fix API key creation timestamp - real value, not hardcoded date
- [x] Fix UI auth prompt - asks for token, no longer falls back silently to "admin"
- [x] nvidia-smi integration - shell exec, XML parse, VRAM total/used, temperature, power draw (mesh host only; remote node GPUs not yet visible)
- [x] Router tests - warm-first logic, model loaded vs not loaded
- [x] Integration tests - mock Ollama HTTP server, routing decision verification
- [x] Zero-config first run - auto-detects localhost:11434, generates API keys, no config.yaml required
- [x] Streaming integration tests - unbuffered delivery, token tracking, mid-stream node death, SSE passthrough

## Phase 2 - Hybrid Routing (complete)

Goal: Cloud fallback. The feature no competitor has.

- [x] Cloud provider config - OpenAI and Anthropic endpoints + keys in config.yaml
- [x] Router fallback logic - when all Ollama nodes busy/down, proxy to configured cloud provider
- [x] Cost tracking - estimated token cost per request (local = $0, cloud = provider rate)
- [x] Savings widget - "$X saved this month vs pure cloud" on the dashboard
- [x] Docker auto-discovery - scans containers with OLLAMA_HOST env or ollama/ollama image, polls every 30s
- [x] Grafana dashboard - 13-panel JSON at grafana/ollama-mesh.json, one-click import
- [x] Admin API versioned at /admin/v1/ - backward compat /admin/ maintained

## v0.2.0 - Dashboard and Observability (complete)

Goal: Full visibility into what the mesh is doing.

- [x] Analytics page - 24-hour area chart (local vs cloud requests), savings stats, per-model breakdown table
- [x] Model catalog page - cross-node VRAM view, warm status badges, node chips, search
- [x] Request log page - live feed with 3-second polling, filter by key/model/status, status indicators
- [x] Rate limit response headers - X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset on every response
- [x] Webhook notifications - node_down and node_up events with HMAC-SHA256 signatures
- [x] Audit logging - append-only JSON-lines file with crypto/rand request IDs
- [x] Prometheus metrics - 11 metrics on :9090/metrics
- [x] GET /health endpoint - unauthenticated, for load balancers and uptime monitors
- [x] X-Request-ID header on all proxy responses

## v0.2.1 - UX and Observability Polish (complete)

Goal: Lower the barrier to first run; make every number in the dashboard trustworthy.

- [x] Real savings math - saved_usd from actual parsed token counts (eval_count + prompt_eval_count); shows null/"—" when unavailable
- [x] Mid-stream abort logging - aborted requests recorded in metrics, admin log, and audit with status="aborted"
- [x] Cloud model rewriting visible in request log - "original -> cloud_model" when default_model is applied
- [x] tokens/sec column in live request log
- [x] VRAM fit indicator - green/yellow/red badges per model per node on GPU Nodes page
- [x] `make demo` - mock Ollama servers in-process, populated dashboard in <60s, no Ollama required
- [x] Configurable savings reference rate - `savings.reference_cost_per_1k` in config.yaml

## Next (planned)

- [ ] Model Advisor page - model catalog with VRAM fit per node, recommend which node to pull a model onto
- [ ] SQLite analytics persistence - survives restarts; deferred until retention semantics are defined
- [ ] Remote node GPU telemetry - sidecar agent for nvidia-smi on non-mesh nodes

## Phase 3 - Enterprise (planned)

Goal: Make it sellable to platform teams at 50-500 person companies.

- [ ] SSO/SAML integration
- [ ] RBAC - per-key model allow-lists enforced at the proxy layer
- [ ] Audit log export - CSV/JSON download via admin API
- [ ] Multi-tenant namespacing
- [ ] Alert manager integration - PagerDuty, OpsGenie
- [ ] Helm chart for Kubernetes deployment

## Phase 4 - Managed Cloud (planned)

Goal: SaaS offering for teams that don't want to self-host the control plane.

- [ ] Hosted control plane
- [ ] Metered billing per token
- [ ] Team management
