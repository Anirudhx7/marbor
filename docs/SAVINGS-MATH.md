# How the savings number is computed

This document explains exactly what the "saved" and "spent" figures on the
dashboard mean, where the numbers come from, and what their limits are.
No marketing. If a number cannot be computed honestly, the dashboard shows
"—" instead of inventing one.

## Savings (local requests)

When a request is served by a local Ollama node, the proxy parses the real
token count from the response:

- Ollama NDJSON: `eval_count + prompt_eval_count` from the final response object
- OpenAI-style SSE: `usage.total_tokens` from the final chunk

The savings for that request are what those tokens would have cost at a
reference cloud rate:

```
saved_usd = (eval_count + prompt_eval_count) / 1000 * reference_cost_per_1k
```

`reference_cost_per_1k` is configurable (see below) and defaults to
**$0.002 per 1K tokens**. It is a single flat rate applied to all locally
served tokens, regardless of which cloud model you would actually have used.
Pick a rate that matches the cloud model you would otherwise pay for; the
default is deliberately conservative.

## Cloud spend (overflow requests)

When a request overflows to a configured cloud provider, spend is computed
from the real token count parsed from the provider's response and that
provider's configured rate:

```
spent_usd = parsed_tokens / 1000 * cost_per_1k_tokens   (per provider, from config)
```

Local requests always count as $0 spend.

## When you see "—"

If requests were served but no token counts could be parsed from any
response (for example, the upstream never sent a final usage object, or the
stream was aborted before the terminal chunk), the API returns `null` and
the dashboard renders "—". The proxy never substitutes an estimated or
random number for missing token data.

## Counters reset on restart

All savings and spend counters are held in memory. Restarting the binary
resets them to zero. There is no persistence of these aggregates yet; the
audit log (JSON lines, if enabled) is the only durable per-request record.

## Changing the reference rate

In `config.yaml`:

```yaml
savings:
  reference_cost_per_1k: 0.002   # USD per 1K tokens
```

Set it to the blended rate of the cloud model your traffic would otherwise
hit (e.g. `0.00015` for gpt-4o-mini, higher for larger models). When the
field is missing or zero, the default `0.002` applies. Changing the rate
only affects requests recorded after the restart; it does not retroactively
revalue earlier requests.

## Known limitations

- One flat reference rate for all local traffic; no per-model reference rates.
- Prompt and completion tokens are valued at the same rate (real cloud
  pricing usually differs between input and output tokens).
- In-memory only; numbers are per-process-lifetime, not per-month, unless
  the process has been up that long.
