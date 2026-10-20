# What happens when every GPU is busy: queueing, timeouts and cloud overflow

When all your GPU nodes are busy, Marbor holds the request in a bounded queue and retries placement as slots free up. If a node becomes available within the queue timeout the request is served; if none does, the client gets a clear `503` instead of hanging. You can also add an optional cloud provider to take overflow, and block that per key.

## The queue

Marbor places a request on a node as soon as one is healthy and under its in-flight cap. When none qualifies, the request waits. Three settings control it, all visible in `marbor settings get`:

| Setting | Default | Meaning |
|---------|---------|---------|
| `routing.queue_max_depth` | 100 | Requests allowed to wait at once |
| `routing.queue_timeout_ms` | 30000 | How long a request waits before it gives up |
| `routing.max_in_flight_per_node` | 0 (no cap) | Global cap on concurrent requests per node |

Change them by writing a partial settings file and applying it:

```bash
cat > queue.json <<'EOF'
{"routing":{"queue_max_depth":50,"queue_timeout_ms":20000,"max_in_flight_per_node":4}}
EOF
marbor settings set --file queue.json
marbor config reload
```

Override the cap for one node, for example a slower card, with `marbor nodes patch vllm-a --max-in-flight 2` (0 clears it back to the global default).

## What the client sees when nothing is available

If no healthy node can take the request before `queue_timeout_ms` elapses, Marbor returns HTTP `503`:

```json
{"error":{"message":"no healthy nodes available","type":"server_error","code":"no_nodes_available"}}
```

The request waited the full queue timeout first, so set `queue_timeout_ms` to the longest delay your clients tolerate, and make clients retry on 503 with backoff.

You can watch the queue live: `marbor_queue_depth` is a Prometheus gauge (see [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)), and failed requests are counted under `marbor_requests_total` with `status="503"` and `node="none"`.

## Optional: overflow to a cloud provider

Marbor can send requests to a cloud API when local capacity is exhausted. Add a provider, set a price per 1K tokens so spend is tracked, and enable it:

```bash
marbor cloud providers add \
  --name openai-overflow --provider openai \
  --base-url https://api.openai.com --api-key sk-... \
  --default-model gpt-4o-mini --cost-per-1k 0.00015 --enabled
marbor cloud providers list
marbor cloud budget-status
```

Overflow is off until you add a provider, so a fresh Marbor never sends a prompt off your network. To keep sensitive workloads on your own hardware, mark their keys local-only; such a key fails closed with a `503` and code `local_only_blocked` rather than reaching a cloud provider:

```bash
marbor key set-local-only data-team true
marbor spill        # per-key counts of requests served locally vs cloud vs blocked
```

Cap what cloud overflow can cost with a key's `--daily-usd-cap` and `--monthly-usd-cap` (see [Per-key quotas](per-key-quotas-self-hosted-llm.html)) and the global caps under **Settings > Cloud Spend Cap**.

## Make busy less likely

- Spread models across nodes so a hot model has more than one place to run: `marbor models fleet` shows where each one is warm.
- Turn on session affinity if your clients send an `X-Session-ID` header, so a conversation keeps hitting the node that already holds its cache (**Settings > Advanced Routing**).
- Check capacity before you add the next model: `marbor fit` says whether a model fits a node's VRAM.

## What to set up next

- Rate limits per key: [Per-key quotas](per-key-quotas-self-hosted-llm.html)
- Alert on queue depth and 503s: [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)
- Take a node out on purpose: [Drain a GPU node](drain-gpu-node-without-downtime.html)
