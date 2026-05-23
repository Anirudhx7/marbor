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
- [x] nvidia-smi integration - shell exec, XML parse, VRAM total/used, temperature, power draw per node
- [x] Router tests - warm-first logic, model loaded vs not loaded
- [x] Integration tests - mock Ollama HTTP server, routing decision verification

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
- [x] Prometheus metrics - 7 metrics on :9090/metrics
- [x] GET /health endpoint - unauthenticated, for load balancers and uptime monitors
- [x] X-Request-ID header on all proxy responses

## Phase 3 - Enterprise (planned)

Goal: Make it sellable to platform teams at 50-500 person companies.

- [ ] SSO/SAML integration
- [ ] RBAC - per-key model allow-lists enforced at the proxy layer
- [ ] Audit log export - CSV/JSON download via admin API
- [ ] Multi-tenant namespacing
- [ ] Persistent analytics - 7-day SQLite store, survives restarts
- [ ] Model pre-warming API - POST /admin/v1/nodes/{name}/pull
- [ ] Alert manager integration - PagerDuty, OpsGenie
- [ ] Helm chart for Kubernetes deployment

## Phase 4 - Managed Cloud (planned)

Goal: SaaS offering for teams that don't want to self-host the control plane.

- [ ] Hosted control plane
- [ ] Metered billing per token
- [ ] Team management
