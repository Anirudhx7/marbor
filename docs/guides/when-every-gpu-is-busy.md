# What happens when every GPU is busy: queueing, timeouts and cloud overflow

When all your GPU nodes are busy, Marbor holds the request in a bounded queue and retries placement as slots free up. If a node becomes available within the queue timeout the request is served; if none does, the client gets a clear `503` instead of hanging. If you added a cloud provider, the request overflows to it at that point instead, and you can block that per key.

## The queue

Marbor places a request on a node as soon as one is healthy and under its in-flight cap. When none qualifies, the request waits. Three settings control it, all visible in `marbor settings get`:

| Setting | Default | Meaning |
|---------|---------|---------|
| `routing.queue_max_depth` | 100 | Requests allowed to wait at once |
| `routing.queue_timeout_ms` | 30000 | How long a request waits before it gives up |
| `routing.max_in_flight_per_node` | 0 (no cap) | Global cap on concurrent requests per node |
| `routing.overflow_sla_ms` | 2000 on a fresh install, 0 once saved as 0 | When a cloud provider can take the request, how long it waits for a busy local node before overflowing to cloud. 0 waits the full queue timeout |

Change them by writing a partial settings file and applying it. The queue depth, queue timeout and overflow SLA take effect on the next request without a restart; the other settings in this table are read once at startup, so apply them with a restart:

```bash
cat > queue.json <<'EOF'
{"routing":{"queue_max_depth":50,"queue_timeout_ms":20000,"max_in_flight_per_node":4}}
EOF
marbor settings set --file queue.json
sudo systemctl restart marbor      # or: docker compose restart marbor
```

Override the cap for one node, for example a slower card, with `marbor nodes patch vllm-a --max-in-flight 2` (0 clears it back to the global default). The per-node override takes effect immediately, no restart.

## What the client sees when nothing is available

If no healthy node can take the request before `queue_timeout_ms` elapses and no cloud provider is enabled, Marbor returns HTTP `503`:

```json
{"error":{"message":"no healthy nodes available","type":"server_error","code":"no_nodes_available"}}
```

The request waited the full queue timeout first, so set `queue_timeout_ms` to the longest delay your clients tolerate, and make clients retry on 503 with backoff.

The 503 carries a `Retry-After` header in whole seconds, equal to your `queue_timeout_ms` rounded up. It is a conservative floor, not a prediction of when a slot frees: the request already waited that long. The header is left out when the queue is disabled or the client disconnected, because there is no honest value to give. A `local_only` key refused with `local_only_blocked` follows the same rule. A model's `rpm`/`tpm` cap answers `429` `model_rate_limited` with `Retry-After` set to the seconds left in the current one-minute window.

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

Overflow is off until you add a provider, so a fresh Marbor never sends a prompt off your network.

Overflow does not happen the instant a node is full. A request for a healthy node that is full first waits, up to `overflow_sla_ms` when a cloud provider can take it (otherwise up to `queue_timeout_ms`), and only then goes to the cloud provider. A request no local node can serve right now (every node is down or draining, or no node serves that model) does not wait at all and goes to the cloud provider immediately; with no cloud provider, a local-only key, or an exhausted cloud budget it waits the full `queue_timeout_ms` as before. If a node frees up during that wait, the request is served locally. A request that overflows is sent with the provider's `--default-model`, not the model name the client asked for, so pick a default model that suits your clients.

To keep sensitive workloads on your own hardware, mark their keys local-only. Such a key waits the full queue timeout like any other request with no cloud, then fails closed with a `503` and code `local_only_blocked` instead of reaching a cloud provider:

```bash
marbor key set-local-only data-team true
marbor spill        # per-key counts of requests served locally vs cloud vs blocked
```

Cap what cloud overflow can cost with a key's `--daily-usd-cap` and `--monthly-usd-cap` (see [Per-key quotas](per-key-quotas-self-hosted-llm.html)) and the global caps under **Settings > Cloud Spend Cap**.

## Keep a model on cloud only

Some models should never run on your GPUs: a licence that forbids it, a model you rent capacity for, or a size you want to keep off reserved hardware. List them as cloud-only and marbor skips local routing and the queue for them and sends them straight to your cloud provider. If cloud cannot take the request (no provider, the spend cap is reached, or an endpoint a cloud provider cannot serve) it gets a `503` and is never run on a local node. A key marked `local_only` also gets a `503` for a listed model, because the two policies contradict.

Set it on the **Routing** page (the **Cloud-only models** card), or through the settings API and CLI. The list is `routing.cloud_only_models`:

```bash
echo '{"routing":{"cloud_only_models":["llama3.3:70b","deepseek-r1*"]}}' > cloud-only.json
marbor settings set --file cloud-only.json
marbor settings get     # shows the list under routing
```

- **Exact names and prefixes.** An entry is an exact model name or a name ending in one `*`. Matching ignores case. An exact `llama3` also matches `llama3:latest` but not `llama3:8b`; a prefix `llama3*` matches `llama3:8b` because that name starts with `llama3`. A bare `*` is not allowed. The name is compared as your client sends it, so `library/llama3` is a different name.
- **The first usable cloud provider is used.** There is no per-model provider choice. With LiteLLM enabled and a URL set, LiteLLM is the whole cloud path; otherwise your enabled cloud providers are used.
- **A save replaces the whole list.** The dashboard and the CLI both write the full list, so the last save wins: if you change the list from two places at the same moment, one change can overwrite the other. Both read the list again right before they write.
- **Saved before it takes effect.** If the save fails the server answers `500` and nothing changes. Changes apply to the next request, with no restart.
- **Empty by default.** With no entries nothing changes for anyone.
- **Automatic warm-ups skip listed models**, so marbor never loads one onto a node on its own.
- **If the stored list cannot be read**, marbor cannot tell which models are listed, so every request that names a model is refused with a `503` (code and routing reason `cloud_only_list_unreadable`) until a list is saved again. The Routing page shows a banner, and `marbor settings get` shows `routing.cloud_only_list_unreadable: true`; that field is read-only and ignored on save. Saving replaces the stored list with exactly what you send, so include every model that must stay cloud-only.
- **Downgrading** to a release that has no cloud-only list ignores the setting.

Requests for a listed model show `cloud_only_policy` in the request log; the refusal caused by an unreadable list shows `cloud_only_list_unreadable`, so an outage reads differently from a deliberate policy refusal.

## Seeing why a request went to cloud

Every request that overflowed to cloud is logged with a reason and two separate timings: how long it waited for a local node, and how long the cloud call took. The values are measured, and show `-` when they were not.

| Reason | Meaning |
|---|---|
| `no_node_serves_model` | No local node serves that model, so it went to cloud without queueing |
| `all_nodes_unavailable` | Every node that could serve it was down or draining, so it went to cloud without queueing |
| `saturated_sla_expired` | Healthy nodes stayed busy past `overflow_sla_ms` |
| `queue_timeout_cloud` | No node freed up within the full queue timeout |
| `queue_full_cloud` | The queue was full or disabled |
| `local_upstream_failed` | A local node took the request and failed it, so it was retried on cloud |
| `local_only_blocked` | The key is `local_only`, so the request got a 503 and never left your network; it records the time it waited but no cloud time |
| `cloud_only_policy` | The model is on the cloud-only list: it went straight to cloud, or got a 503 because cloud could not take it, and never ran on a local node |
| `cloud_only_list_unreadable` | The stored cloud-only list could not be read, so a request naming a model was refused with a 503 until a list is saved again |

See them with `marbor requests` (WAIT MS and CLOUD MS columns, REASON), `marbor requests explain <id>` for the sentence, the Requests page in the dashboard, or the `marbor_cloud_overflow_reason_total{reason}` metric.

## Make busy less likely

- Spread models across nodes so a hot model has more than one place to run: `marbor models fleet` shows where each one is warm.
- Turn on session affinity if your clients send an `X-Session-ID` header, so a conversation keeps hitting the node that already holds its cache (**Settings > Routing & Reliability**).
- Check capacity before you add the next model: `marbor fit` says whether a model fits a node's VRAM.

## What to set up next

- Rate limits per key: [Per-key quotas](per-key-quotas-self-hosted-llm.html)
- Alert on queue depth and 503s: [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)
- Take a node out on purpose: [Drain a GPU node](drain-gpu-node-without-downtime.html)
