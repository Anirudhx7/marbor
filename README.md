<!-- generated-by: gsd-doc-writer -->
# ollama-mesh

**GPU-aware Ollama load balancer with cloud fallback. Route to warm VRAM first, fall back to OpenAI/Anthropic when busy.**

[![Build Status](https://github.com/Anirudhx7/ollama-mesh/actions/workflows/ci.yml/badge.svg)](https://github.com/Anirudhx7/ollama-mesh/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

---

## What's New in v0.2.0

- Analytics page: 24-hour local vs cloud chart, savings stats, per-model breakdown
- Model catalog: see which models are warm across all nodes, with VRAM usage
- Request log: live feed of all requests with filter and status indicators
- Rate limit headers: X-RateLimit-Limit/Remaining/Reset on every response
- Webhook alerts: node_down/node_up events signed with HMAC-SHA256
- Audit logging: append-only JSON-lines with request IDs

See [CHANGELOG.md](CHANGELOG.md) for the full list.

---

## Why

- You have multiple Ollama nodes but no way to load balance them
- Models cold-start on every new node - 30 second delays kill UX
- When your GPU cluster is full, requests just fail with no fallback
- You have no visibility into which GPU is serving what, or what it's costing you

---

## What It Does

| Feature | Detail |
|---------|--------|
| Warm-first routing | Routes to the node that already has the model in VRAM. Eliminates cold starts. |
| Cloud fallback | When all GPUs are busy or down, automatically routes to OpenAI or Anthropic. |
| Savings tracking | Shows exactly how much you saved vs pure cloud this month. |
| Docker auto-discovery | Detects `ollama/ollama` containers automatically from the Docker socket. Zero config. |
| Real nvidia-smi metrics | Actual VRAM usage, temperature, and power draw per node - not fake numbers. |
| API key management | Per-key rate limits, model allow-lists, and key expiry. |
| Prometheus metrics | 7 metrics exposed at `:9090`. Grafana dashboard included. |
| Analytics dashboard | 24-hour area chart, savings stats, per-model breakdown. |
| Model catalog | Cross-node VRAM view with warm status and search. |
| Request log | Live feed with 3-second polling, filter, and status badges. |
| Webhook alerts | node_down/node_up events with HMAC-SHA256 signatures. |
| Rate limit headers | X-RateLimit-Limit/Remaining/Reset on every response. |
| Zero dependencies | Single Go binary. Runs anywhere. No Python, no Node, no runtime. |

---

## Quick Start

**Binary (Linux/macOS):**
```bash
curl -Lo ollama-mesh https://github.com/Anirudhx7/ollama-mesh/releases/latest/download/ollama-mesh-linux-amd64
chmod +x ollama-mesh
cp config.example.yaml config.yaml
./ollama-mesh
```

**Docker Compose:**
```bash
git clone https://github.com/Anirudhx7/ollama-mesh
cd ollama-mesh
cp config.example.yaml config.yaml
docker-compose up -d
```

**Build from source:**
```bash
git clone https://github.com/Anirudhx7/ollama-mesh
cd ollama-mesh
make build
./ollama-mesh
```

Point your Ollama clients at `:11434` instead of a single node. Everything else stays the same.

---

## Configuration

Start from `config.example.yaml`:

```yaml
proxy:
  port: 11434
  log_level: info

# Your Ollama nodes
nodes:
  - name: gpu-0
    url: http://localhost:11435
    gpu_model: "NVIDIA RTX 4090 24GB"
  - name: gpu-1
    url: http://localhost:11436
    gpu_model: "NVIDIA RTX 4090 24GB"

# warm-first routes to nodes that have the model in VRAM.
# Falls back to least-connections when no warm node is available.
routing:
  strategy: warm-first
  poll_interval_ms: 2000
  fallback: least-connections

# Per-key rate limits and model allow-lists
auth:
  enabled: true
  keys:
    - name: team-shared
      key: sk-mesh-xyz789
      rate_limit: 5000
      models: []             # empty = allow all models
    - name: agent-runner
      key: sk-mesh-agent001
      rate_limit: 500
      expires_at: "2027-01-01"

metrics:
  enabled: true
  port: 9090

# Cloud fallback: only used when all local nodes are unavailable
cloud_providers:
  - name: openai-fallback
    provider: openai
    base_url: https://api.openai.com
    api_key: sk-...
    default_model: gpt-4o-mini
    cost_per_1k_tokens: 0.00015
    enabled: false

# Docker auto-discovery
docker:
  enabled: false
  socket: /var/run/docker.sock
  poll_interval_ms: 30000
```

---

## How Routing Works

```
Request
  |
  v
Auth (API key validation + rate limit)
  |
  v
Extract model name from JSON body
  |
  +-- Model warm in VRAM? --Yes--> Route to warm node (least connections)
  |                                         |
  |                                         v
  |                                Stream response
  |
  +-- No warm node --> All nodes healthy? --Yes--> Route to least-connections node
                               |
                               +-- All busy/down? --> Cloud fallback (OpenAI/Anthropic)
                                                              |
                                                              v
                                                   Log cost + update savings tracker
```

The router polls `/api/ps` on each node every 2 seconds to know which models are loaded in VRAM. No guessing, no stale state.

---

## Servers

| Port | Purpose |
|------|---------|
| `:11434` | Ollama-compatible proxy - drop-in replacement for a single Ollama instance |
| `:8080` | Admin dashboard + REST API |
| `:9090` | Prometheus metrics |

---

## Admin API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/v1/nodes` | Node list with status, VRAM usage, and active models |
| GET | `/admin/v1/keys` | API key list with usage stats |
| GET | `/admin/v1/metrics/savings` | Cost savings vs pure cloud this month |
| GET | `/admin/v1/cloud/providers` | Configured cloud fallback providers and status |
| GET | `/health` | Returns 200 when proxy is ready |

---

## Cloud Fallback Setup

Set `enabled: true` on any provider in `config.yaml`:

```yaml
cloud_providers:
  - name: openai-fallback
    provider: openai
    base_url: https://api.openai.com
    api_key: sk-...
    default_model: gpt-4o-mini
    cost_per_1k_tokens: 0.00015
    enabled: true

  - name: anthropic-fallback
    provider: anthropic
    base_url: https://api.anthropic.com
    api_key: sk-ant-...
    default_model: claude-haiku-4-5-20251001
    cost_per_1k_tokens: 0.00025
    enabled: true
```

Cloud providers are used **only** when all local Ollama nodes are unavailable or at capacity. Local GPU always wins when it can serve the request.

---

## Docker Auto-Discovery

Enable in `config.yaml`:

```yaml
docker:
  enabled: true
  socket: /var/run/docker.sock
  poll_interval_ms: 30000
```

ollama-mesh scans the Docker socket every 30 seconds and registers any running `ollama/ollama` containers as nodes automatically. No manual node entries required.

When running ollama-mesh itself in Docker, mount the socket:

```yaml
services:
  ollama-mesh:
    build: .
    ports:
      - "11434:11434"
      - "8080:8080"
      - "9090:9090"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./config.yaml:/root/config.yaml
```

---

## Grafana

Import `grafana/ollama-mesh.json` into Grafana and point the Prometheus datasource at `:9090`. The dashboard shows VRAM utilization per node, request throughput, latency percentiles, and cloud fallback rate.

---

## Roadmap

- [x] Phase 1: Trustworthy - real nvidia-smi GPU metrics, no fake data, mutex-safe auth
- [x] Phase 2: Hybrid routing - cloud fallback, savings tracking, Docker auto-discovery
- [x] v0.2.0: Analytics, model catalog, request log, webhooks, rate limit headers
- [ ] Phase 3: Enterprise - SSO, RBAC, audit log export, Helm chart
- [ ] Phase 4: Managed cloud - metered token hosting

---

## Contributing

PRs welcome. Run `go test ./...` before submitting.

---

## License

MIT
