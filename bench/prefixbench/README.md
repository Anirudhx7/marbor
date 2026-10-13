# prefixbench

Measures whether prefix-aware routing helps, using the same traffic against each routing
arm, interleaved across repetitions so slow hardware drift cannot favor one arm.

## What it compares

Preset `prefix` (three arms, in this order; the first is the baseline):

| arm | how requests are routed |
|---|---|
| `load-only` | through a fresh router with `prefix_locality_enabled=false` |
| `prefix-only` | the harness hashes the conversation head straight to an engine, no load awareness, router bypassed |
| `prefix-plus-load` | through a fresh router with `prefix_locality_enabled=true` and the given weight |

The shipped router cannot express a prefix-only policy (the prefix weight is capped below the
load weight), so that arm is realized outside the router. It also skips the router's proxy hop,
so differences between it and the router arms include that hop as well as the policy.

Preset `loadspread` compares two router binaries (`--arm-bin baseline=PATH --arm-bin candidate=PATH`)
on the same engines, typically with the `unique` workload at higher concurrency.

## Workloads

- `conv`: many multi-turn conversations, each with its own long first message.
- `shared`: conversations that all start with the same long system prompt. The router keys on
  the first 200 characters of the first message, so these collapse to one key.
- `unique`: single-turn requests with unique content. The control: nothing to reuse.

A per-run tag at the head of every conversation keeps runs from reusing each other's cached
prefixes. Each run starts a fresh router process and database.

## What counts as evidence

Each run records every request and two independent sources of truth: the router's own
per-request routing explanation, and the engines' prefix-cache counters (vLLM
`prefix_cache_queries/hits` metrics). Validity flags mark runs whose numbers may not mean what
the arm name says: `high_errors`, `collapsed_placement`, `low_explain_coverage`,
`no_engine_evidence`.

Results are paired per repetition against the baseline arm: median delta, bootstrap 95%
interval, reps improved or worsened, and an exact sign test. Ten repetitions give a coarse
interval. A verdict is stated only when a criteria file written before the run is passed with
`--criteria`; its sha256 is recorded in `config.json`.

## Commands

```bash
go build -o prefixbench ./bench/prefixbench

# list the interleaved schedule without running anything
./prefixbench plan --engines a=http://HOST:8000,b=http://HOST:8001 --model MODEL --router-bin ./marbor --out out/run1

# run (resumable: rerun the same command to continue after an interruption)
./prefixbench run  --engines a=http://HOST:8000,b=http://HOST:8001 --model MODEL --router-bin ./marbor --out out/run1 --criteria criteria.json

# rebuild the summary from raw records
./prefixbench summarize out/run1

# save raw engine responses to see which usage and cache fields a runtime returns
./prefixbench capture --url http://HOST:8000 --model MODEL --out out/capture
```

`--engine-kind sim` reads the counters of the simulated engine in `cmd/mocknode` (started with
`SIM_KV=1`). That simulation exists only to test the harness with a planted effect and a
no-effect control. Its numbers are never measurements, and the summary says so.

`--smoke` allows fewer than ten repetitions for harness checks.

## Output directory

`config.json` (arguments, host, binary hashes, criteria hash), `runs.jsonl` (per-run metadata,
engine counter readings, router log tail), `requests/runNNN.jsonl` (every request),
`summaries.json`, `paired.json`, `summary.md`, and `verdict.json` when criteria were given.
