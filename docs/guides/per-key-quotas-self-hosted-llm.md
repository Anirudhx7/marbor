# Per-key quotas and rate limits for a self-hosted LLM gateway

Marbor issues its own API keys in front of your GPU servers, and each key can carry an hourly rate limit, daily and monthly request quotas, a model allow-list, and an expiry date. Your vLLM, Ollama, llama.cpp or MLX servers never see the keys; Marbor checks them before a request is placed on any node, so one noisy app or teammate cannot take the whole fleet.

This guide assumes Marbor is installed and logged in (`marbor login`). If not, start with [Load balance multiple vLLM servers](load-balance-vllm-servers.html) or [One endpoint for several Ollama machines](one-endpoint-for-several-ollama-machines.html).

## Create a key with limits

```bash
marbor key create --name data-team \
  --rate-limit 600 \
  --daily-limit 5000 \
  --monthly-limit 100000 \
  --models "meta-llama/Llama-3.1-8B-Instruct,qwen2.5:7b" \
  --expires-at 2027-03-31
```

The plaintext key (`sk-data-team-...`) prints once and is stored encrypted afterwards, so copy it now. Flags, all optional:

| Flag | Meaning |
|------|---------|
| `--rate-limit` | Maximum requests per hour (0 = unlimited) |
| `--daily-limit` / `--monthly-limit` | Maximum requests per day / month (0 = unlimited) |
| `--models` | Comma-separated allow-list; empty means every model |
| `--expires-at` | Expiry date, `2006-01-02` or RFC3339 |
| `--key` | Supply your own secret instead of a generated one |

Change a key later without reissuing it, and revoke one when someone leaves:

```bash
marbor key patch data-team --daily-limit 10000
marbor key revoke data-team --yes
marbor key list
```

## What clients see when a limit trips

Marbor answers with an OpenAI-style error body, so SDKs surface a readable message:

| Situation | HTTP status | `error.code` |
|-----------|-------------|--------------|
| No or malformed `Authorization` header | 401 | `missing_auth_header` |
| Hourly rate limit hit | 429 (with a `Retry-After` header) | `rate_limit_exceeded` |
| Daily request quota used up | 429 | `daily_quota_exceeded` |
| Model not on the key's allow-list | 403 | `model_not_allowed` |

Test it before you hand a key out. A key created with `--daily-limit 3` accepts three requests and rejects the fourth:

```bash
for i in 1 2 3 4; do
  curl -s -o /dev/null -w "%{http_code}\n" http://<marbor-host>:11434/v1/chat/completions \
    -H "Authorization: Bearer sk-data-team-..." -H "Content-Type: application/json" \
    -d '{"model":"meta-llama/Llama-3.1-8B-Instruct","messages":[{"role":"user","content":"hi"}]}'
done
```

Expect `200 200 200 429`.

## Spend caps are for cloud overflow only

`--daily-usd-cap` and `--monthly-usd-cap` cap what a key may spend on cloud overflow providers, not on your own GPUs. They matter only if you have configured a cloud fallback (see [When every GPU is busy](when-every-gpu-is-busy.html)). Local requests are limited by the request counts above.

## Watch rejections

With metrics enabled (see [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)), the counter `marbor_quota_rejections_total` rises each time a key is turned away, and `marbor_requests_total` carries a `key_name` label so you can chart usage per key. In the dashboard, the **API Keys** page shows each key's requests today and this month.

## A common layout

- One key per application, not per person, so a leaked key is one revocation.
- A tight allow-list on keys handed to untrusted tools, for example only the small model.
- An expiry date on every contractor or trial key.
- An admin-only key with no limits for your own scripts.

## What to set up next

- What happens at the edge of capacity: [When every GPU is busy](when-every-gpu-is-busy.html)
- Charts per key: [Monitor a self-hosted LLM fleet](monitor-llm-fleet-prometheus-grafana.html)
- Front an existing chat UI with a key: [Open WebUI integration](../integrations/open-webui.html)
