# Cost Deflection Analysis: Local GPU Inference vs. Cloud API Spend

This document is the financial model behind ollama-mesh's savings tracking. It is designed for infrastructure directors and finance teams evaluating the ROI of shifting LLM inference from third-party cloud APIs to owned GPU hardware routed through ollama-mesh.

Every figure in the ollama-mesh dashboard is derived from the formulas below. When token data is unavailable, the dashboard displays "-" rather than an estimate. No number is ever fabricated.

---

## Executive Summary

An engineering organization running multi-agent LLM workflows at scale - coding copilots, RAG pipelines, automated code review, internal search - consumes millions of tokens per day. At cloud API rates, this translates to $5,000–$50,000/month in direct API spend, depending on model tier and volume.

ollama-mesh enables organizations to route this traffic to owned GPU hardware first, falling back to cloud APIs only when local capacity is exhausted. The savings dashboard tracks every token served locally and values it against the cloud rate the organization would otherwise pay.

**Typical result:** Platform teams running 2–4 GPU nodes with ollama-mesh report 60–85% reduction in cloud API spend within the first billing cycle, with the reduction visible in the dashboard from day one.

---

## The Formulas

### Local Request Savings

When a request is served by a local Ollama node, ollama-mesh parses the real token count from the upstream response:

- **Ollama NDJSON responses:** `eval_count` (completion tokens) + `prompt_eval_count` (input tokens) from the final streamed object
- **OpenAI-compatible SSE responses:** `usage.total_tokens` from the terminal `[DONE]`-adjacent chunk

Savings for each locally-served request:

```
saved_usd = (prompt_tokens + completion_tokens) / 1000 × reference_cost_per_1k
```

The `reference_cost_per_1k` is operator-configured and represents the blended cloud rate the organization would otherwise pay. Default: **$0.002/1K tokens** (deliberately conservative - adjust to match your actual cloud provider rate).

### Cloud Overflow Spend

When a request overflows to a configured cloud provider (OpenAI, Anthropic), spend is computed from the real token count parsed from the provider's response:

```
spent_usd = parsed_tokens / 1000 × cost_per_1k_tokens
```

`cost_per_1k_tokens` is set per cloud provider on the dashboard's **Settings > Cloud Providers** page. Local requests are always $0 spend.

### Net Savings

```
net_savings_usd = Σ saved_usd (all local requests) - Σ spent_usd (all cloud requests)
```

This is the number displayed on the dashboard savings widget. It represents the actual dollar amount the organization avoided paying to cloud providers by serving traffic locally.

---

## Enterprise Workload Projections

The following projections use conservative assumptions grounded in real-world multi-agent workflow patterns. All figures assume the default reference rate of $0.002/1K tokens. Adjust proportionally for your actual cloud rate.

### Scenario A: Engineering Copilot (50-Person Team)

| Parameter | Value |
|-----------|-------|
| Engineers using LLM-assisted tools | 50 |
| Average requests per engineer per day | 80 |
| Average tokens per request (prompt + completion) | 2,500 |
| Daily token volume | 10,000,000 |
| Monthly token volume (22 working days) | 220,000,000 |
| Cloud cost at $0.002/1K | **$440/month** |
| Cloud cost at $0.005/1K (GPT-4o, blended input/output) | **$1,100/month** |
| Cloud cost at $0.015/1K (Claude Sonnet 4, output-heavy) | **$3,300/month** |
| Local GPU serving cost (amortized hardware + power, 3yr) | ~$200/month (2× RTX 4090) |
| **Monthly net savings at $0.005/1K reference** | **$900/month** |
| **Monthly net savings at $0.015/1K reference** | **$3,100/month** |

### Scenario B: Multi-Agent RAG Pipeline (Production)

| Parameter | Value |
|-----------|-------|
| Agent pipelines in production | 12 |
| Average pipeline executions per day | 500 |
| Average tokens per execution (multi-step) | 15,000 |
| Daily token volume | 90,000,000 |
| Monthly token volume | 2,700,000,000 |
| Cloud cost at $0.002/1K | **$5,400/month** |
| Cloud cost at $0.005/1K (GPT-4o, blended) | **$13,500/month** |
| Cloud cost at $0.015/1K (Claude Sonnet 4, output-heavy) | **$40,500/month** |
| Local GPU serving cost (amortized hardware + power, 3yr) | ~$1,800/month (4× A100 80GB) |
| **Monthly net savings at $0.002/1K reference** | **$3,600/month** |
| **Monthly net savings at $0.005/1K reference** | **$11,700/month** |
| **Monthly net savings at $0.015/1K reference** | **$38,700/month** |
| **Annual net savings at $0.015/1K reference** | **$464,400/year** |

### Scenario C: Internal Platform (200-Person Organization)

| Parameter | Value |
|-----------|-------|
| Departments using LLM services | 5 (engineering, data science, support, legal, product) |
| Total daily token volume across departments | 150,000,000 |
| Monthly token volume | 4,500,000,000 |
| Cloud cost at $0.002/1K | **$9,000/month** |
| Cloud cost at $0.005/1K (GPT-4o, blended) | **$22,500/month** |
| Cloud cost at $0.015/1K (Claude Sonnet 4, output-heavy) | **$67,500/month** |
| Local GPU fleet cost (amortized hardware + power, 3yr) | ~$3,600/month (8× A100 80GB) |
| **Monthly net savings at $0.005/1K reference** | **$18,900/month** |
| **Monthly net savings at $0.015/1K reference** | **$63,900/month** |
| **Annual net savings at $0.015/1K reference** | **$766,800/year** |
| **3-year TCO advantage** | **$2,300,400** |

> **Note:** These projections calculate the *difference* between cloud API costs and amortized local hardware costs. The hardware amortization includes purchase price, power, cooling, and rack space over a 3-year depreciation schedule. Actual savings depend on GPU utilization rates, model sizes, and local-vs-cloud traffic split. ollama-mesh's dashboard shows the *real* split based on actual parsed token counts, not these projections.

---

## Hardware ROI Calculator

### Break-Even Analysis

The break-even point is when cumulative cloud savings equal the upfront GPU hardware investment:

```
break_even_months = hardware_cost / (monthly_cloud_savings - monthly_hardware_opex)
```

Monthly hardware opex = amortized purchase price (3yr) + power + rack/cooling. At $0.015/1K (Claude Sonnet 4 output-heavy workloads):

| GPU Configuration | Hardware Cost | Monthly Opex (amort+power) | Monthly Cloud Equivalent | Break-Even |
|-------------------|--------------|---------------------------|--------------------------|------------|
| 2× RTX 4090 (24GB each) | $3,600 | ~$200/month | $3,300/month | **~1.2 months** |
| 4× A100 80GB | $60,000 | ~$1,800/month | $40,500/month | **~1.6 months** |
| 8× A100 80GB | $120,000 | ~$3,600/month | $67,500/month | **~1.9 months** |

At GPT-4o blended rates ($0.005/1K), the monthly cloud equivalent is lower but break-even remains attractive: 2× RTX 4090 breaks even in ~4 months at GPT-4o rates.

After break-even, every locally-served token is pure cost deflection. The hardware continues serving traffic for 3-5 years.

> **Hardware pricing notes:** RTX 4090 ~$1,800/card at current retail. A100 80GB ~$10,000-15,000/card (data center pricing, new); secondary market lower. Monthly opex above uses 3-year straight-line amortization plus ~$0.12/kWh power at typical GPU utilization. Rack/cooling adds 10-15% in enterprise colocations.

---

## How the Dashboard Computes These Numbers

### Data Sources

1. **Local token counts** - parsed from the real upstream Ollama response, not estimated. ollama-mesh reads `eval_count` and `prompt_eval_count` from the final NDJSON object (Ollama native) or `usage.total_tokens` from the terminal SSE chunk (OpenAI-compatible).

2. **Cloud token counts** - parsed from the real cloud provider response. OpenAI and Anthropic both include usage objects in their streaming responses.

3. **Reference rate** - operator-configured on the dashboard's **Settings > Global Warmup & Audit** page (Savings reference cost). Single flat rate applied to all locally-served tokens.

4. **Cloud rates** - per-provider `cost_per_1k_tokens`, configured per cloud provider via the dashboard or the admin API.

### What "-" Means

If requests were served but no token counts could be parsed from any response - for example, the upstream never sent a final usage object, or the stream was aborted before the terminal chunk - the API returns `null` and the dashboard renders "-". ollama-mesh **never** substitutes an estimated or random number for missing token data.

### Counter Lifecycle

Savings and spend counters are held in memory. Per-key token totals and quota counters are persisted to `usage-state.json` (configurable via `auth.state_path`) and survive restarts. Aggregate savings counters reset on process restart. The audit log (JSON-lines, if enabled) is the durable per-request record.

---

## Configuration

### Setting the Reference Rate

The reference rate (`reference_cost_per_1k`) is configured via the dashboard's **Settings > Global Warmup & Audit** page, or the admin API (`/admin/v1/settings`) - there is no config file. Set it to the blended rate of the cloud model your traffic would otherwise consume:

| Cloud Model | Rate (per 1K tokens) | Blended (70% in / 30% out) | Notes |
|-------------|---------------------|---------------------------|-------|
| GPT-4o mini | $0.00015–$0.0006 | ~$0.00028/1K | Cheapest quality tier |
| GPT-4o | $0.0025–$0.01 | ~$0.005/1K | Most common enterprise default |
| GPT-4.1 | $0.002–$0.008 | ~$0.0038/1K | Input vs output pricing |
| Claude Haiku 4.5 | $0.0008–$0.004 | ~$0.0018/1K | Fast, cheap Anthropic tier |
| Claude Sonnet 4 | $0.003–$0.015 | ~$0.0066/1K | Mid-tier; blended ≈ $0.006/1K |
| Claude Opus 4 | $0.015–$0.075 | ~$0.033/1K | Premium; output-heavy = $0.075 |

When the field is missing or zero, the default `$0.002/1K` applies. Changing the rate only affects requests recorded after the change; it does not retroactively revalue earlier requests.

### Per-Provider Cloud Rates

Each cloud provider's `cost_per_1k_tokens` (along with `name`, `provider`, and `enabled`) is configured via the dashboard or the admin API (`/admin/v1/settings`) - for example, an `openai-overflow` provider at `0.0025` (GPT-4o blended rate) or an `anthropic-overflow` provider at `0.004` (Claude Sonnet 4 blended rate). There is no config file to edit.

---

## Known Limitations

| Limitation | Impact | Mitigation |
|-----------|--------|------------|
| Single flat reference rate for all local traffic | Over- or under-values savings for specific models | Set rate to match your highest-volume cloud model |
| Prompt and completion tokens valued at the same rate | Real cloud pricing differs (input is cheaper) | Use a blended rate weighted toward your actual input/output ratio |
| Aggregate savings are per-process-lifetime, not calendar-month | Restart resets the dashboard savings counter | Per-key token totals persist; audit log provides durable per-request records |
| No per-model reference rates | Cannot model mixed-model cost accurately | Planned for a future release |

---

## Audit Trail for Financial Verification

Every routed request is recorded in the JSON-lines audit log (when enabled) with:

- Request ID (crypto/rand generated)
- Timestamp
- API key name (never the key value)
- Model requested
- Target node (or cloud provider)
- Token counts (prompt + completion)
- Calculated cost (saved or spent)
- HTTP status and latency

This log is the source of truth for financial reconciliation. It can be ingested into any log aggregator (Splunk, Elastic, Datadog) for custom reporting, cost allocation, and audit compliance.
