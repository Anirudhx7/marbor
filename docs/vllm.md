# Production vLLM without Kubernetes

One OpenAI-compatible endpoint, health-aware routing, and drain without downtime across your vLLM servers.

vLLM provides fast throughput, PagedAttention, and continuous batching. However, moving from a single vLLM server to multiple GPU instances often pushes operators toward complex Kubernetes deployments, custom operators, and distributed ingress stacks. If you have 4 to 20 GPUs on dedicated bare-metal or cloud instances, you need high availability and model placement without the operational burden of Kubernetes.

Marbor provides the missing operational layer. It sits in front of your vLLM instances as a single Go binary, health-checks each engine using native `/health` endpoints, routes requests based on live model residency and VRAM headroom, and lets you drain nodes for upgrades without dropping client traffic.

## Quick start for vLLM clusters

### 1. Start Marbor via Docker Compose

```bash
git clone https://github.com/Anirudhx7/marbor && cd marbor
docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d
```

This starts the Marbor gateway on port 8080 (admin) and port 11434 (inference proxy), alongside pre-configured Prometheus and Grafana monitoring.

### 2. Register your vLLM nodes

Add your running vLLM servers via the CLI or admin API:

```bash
marbor nodes add vllm-node-1 http://10.0.1.20:8000 \
  --runtime vllm \
  --gpu-model "RTX 4090" \
  --vram-total-mb 24576
```

Or via HTTP:

```bash
curl -X POST http://localhost:8080/admin/nodes \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"vllm-node-1","url":"http://10.0.1.20:8000","runtime":"vllm","gpu_model":"RTX 4090","vram_total_mb":24576}'
```

### 3. Point your application at the unified endpoint

```bash
export OPENAI_BASE_URL=http://<marbor-host>:11434/v1
export OPENAI_API_KEY=<marbor-api-key>
```

Clients send standard OpenAI-format completions to `http://<marbor-host>:11434/v1/chat/completions`.

## Architecture: Marbor alongside LiteLLM

If your organization already uses LiteLLM as an enterprise gateway for cloud providers, keep it:

```text
Apps -> LiteLLM (cloud gateway) -> Marbor (GPU control plane) -> vLLM nodes
```

LiteLLM handles organization-level cloud billing and multi-provider abstraction; Marbor handles local hardware placement, live VRAM telemetry, runtime health checks, and node draining.

## Three operational outcomes shipped today

1. **Inference-aware placement vs. blind round-robin**: Traditional reverse proxies (nginx, HAProxy) route TCP connections or HTTP requests round-robin, unaware of whether an instance is saturated or evicting cache. Marbor polls each node every 2 seconds, tracks live in-flight requests, and prefers nodes with warm model residency and available VRAM headroom.
2. **Graceful drain without dropped requests**: Taking a vLLM node down for driver updates, weight reloads, or host maintenance no longer causes 502 errors. Call `marbor nodes drain <node>`, and Marbor diverts new requests to alternate nodes while allowing existing generation streams to finish.
3. **Runtime-aware health checks**: Marbor continuously probes the native `/health` endpoint of each vLLM instance rather than relying on shallow TCP port checks, ensuring that an engine undergoing initialization or memory recovery is never routed live traffic.

## Roadmap

- Prefix-locality affinity: routing prompt prefixes to specific vLLM instances to maximize KV-cache reuse (redesign in progress).
- LoRA adapter-aware routing: tracking active adapter weights per node alongside base models.
- Standby node sleep-mode orchestration for idle power reduction.

## When NOT to use Marbor

- **Single vLLM instance**: If you run a single vLLM process on one GPU host, connect your applications directly to that server.
- **100+ GPU Kubernetes clusters**: If your organization has standardized on Kubernetes operators (such as KubeRay or vLLM Production Stack on K8s), use those platform tools.
- **Cloud-only API setups**: If you do not operate self-hosted GPU hardware, an on-prem control plane is unnecessary.

## Next steps

- Step-by-step tutorial: [Load balance multiple vLLM servers](guides/load-balance-vllm-servers.md)
- Zero-downtime maintenance: [Drain a GPU node without downtime](guides/drain-gpu-node-without-downtime.md)
- Prometheus metrics: [Monitor a fleet with Prometheus and Grafana](guides/monitor-llm-fleet-prometheus-grafana.md)
