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

## Single-engine probe

`plan` and `run` compare routing arms and need two or more engines. `probe` is the one mode that
measures a single engine, with no router: how much faster a request whose prefix is cached is than
one whose prefix is not, whether that gap holds under load, and how many distinct conversations
the engine keeps resident before revisits stop hitting its prefix cache.

```bash
./prefixbench probe --engine e=http://HOST:8000 --model MODEL --out out/probe
```

Steps (`--steps size,idle,load,residency`, all by default):

- `size`: one 5-turn conversation, recording `prompt_tokens` at turn 1 and the last turn and whether
  the last turn plus `--max-tokens` fits `--max-model-len` (default 8192; set it to the engine's value).
- `idle`: `--probes` conversations (30), each sent as turn 1 (prefix never seen) then turn 2 on the same
  prefix, nothing else running. Reports `ttft_u_*` (uncached) and `ttft_c_*` (cached) p50 and mean,
  and `g = (ttft_u - ttft_c) / ttft_u` on the mean (`g_p50` on the median). Only complete pairs
  count: if a conversation's turn 1 fails it is not continued and not scored.
- `load`: the same pairs while a closed loop of unique single-turn requests holds each `--levels`
  concurrency (12, 24, 48), so `--levels 12` has 12 background requests plus the probe request in
  flight. Probing starts once every background worker has completed a successful request; if that
  does not happen within 60 seconds the cell gets `load_warm: false` and a warning.
  Levels with `g >= 0.10` are listed as `load_levels_g_ge_gain_line`. Background requests are counted
  (`load_requests`, `load_errors`, `load_first_error`) but not written to `requests.jsonl`.
- `residency`: for each N in `--residency` (8 to 256), N distinct first turns, then all N revisited
  in the same order. `engine_cached_frac` is the engine's own hit fraction over the revisit phase
  only, next to the share of revisits whose reported `cached_tokens` covers at least half the prompt.
  `revisits` counts scored revisits (successful and reporting usage). `k` is the last N before the first
  level whose engine hit fraction falls below 0.90: for example `16`, `>256` when every requested level
  passes, `<8` when the first level already fails, `-` when any requested level lacks engine counters
  or the run was interrupted. A level that passes after a lower one failed is reported as a warning,
  not as capacity. `revisit_ceiling` is the highest fraction a perfect cache could report (first-turn tokens
  over revisit-turn tokens); a warning is printed when it is below 0.90, because `k` then reflects the
  prompt shape rather than residency.

A statistic that could not be observed (no complete pairs, no usage in the response, no engine
counters) is `null` in `probe.json` and `-` in `probe.md`; it is never estimated. Each conversation's
first message starts with a conversation-specific token and every prompt carries a per-invocation
`--nonce`, so neither another conversation nor an earlier run of the same engine can pre-warm a prefix.
`--label` records free text such as the memory setting under test. Ctrl-C or SIGTERM stops
promptly, still writes what was measured, and marks it `interrupted`.

Output: `probe.json`, `probe.md` and `requests.jsonl`; an `--out` that already holds a `probe.json` is
refused. With `--engine-kind sim` or `--smoke` the
output is marked not a measurement. The probe talks to the engine directly and sends no API key, so it
cannot probe an engine that requires one.

## Output directory

`config.json` (arguments, host, binary hashes, criteria hash), `runs.jsonl` (per-run metadata,
engine counter readings, router log tail), `requests/runNNN.jsonl` (every request),
`summaries.json`, `paired.json`, `summary.md`, and `verdict.json` when criteria were given.
