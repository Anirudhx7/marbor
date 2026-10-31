# GuideLLM benchmark runner

Runs one fixed profile matrix against one OpenAI-compatible endpoint, so a
comparison across proxies is one command per contender with identical load.
Load generation is GuideLLM; nothing here is a hand-rolled generator.

Pinned: GuideLLM 0.8.1. Runner and summary are Python 3.9+, standard library
only. Raw results are written to a directory you choose and are not meant to
be committed.

## Files

| File | Purpose |
|---|---|
| `runner.py` | `verify`, `freeze`, `run`, `env`, `summary` subcommands |
| `matrix.json` | seed, token shapes, warmup, profiles and request counts |
| `slo.json` | TTFT and ITL targets per token shape, frozen before measured runs |
| `slo.sha256` | recorded checksum of `slo.json`, checked by `verify` and by `run` |
| `summary.py` | contender x profile table from GuideLLM JSON |
| `Dockerfile` | GuideLLM 0.8.1 image, for hosts where a native install is not possible |

## Install GuideLLM

Native (Linux):

```bash
python3 -m venv ~/guidellm && ~/guidellm/bin/pip install "guidellm==0.8.1"
export PATH=~/guidellm/bin:$PATH
guidellm --version
```

Container (Windows, or any host without a native install; `uvloop` does not
build on Windows). The image is large (about 6 GB, GuideLLM pulls torch):

```bash
docker build -t marbor-guidellm:0.8.1 bench/guidellm
```

## Run

Freeze the targets once, before the first measured run, and commit
`slo.sha256` with `slo.json`:

```bash
python3 bench/guidellm/runner.py freeze
python3 bench/guidellm/runner.py verify
```

One contender, native:

```bash
GUIDELLM_API_KEY=... python3 bench/guidellm/runner.py run \
  --endpoint http://HOST:PORT --model Qwen/Qwen2.5-7B-Instruct \
  --contender marbor --out /path/to/results --run-kind MEASURED \
  --cross-check --note vllm=X.Y.Z --note region=...
```

One contender, through the container (the endpoint must be reachable from the
container; use `host.docker.internal` for a service on the Docker host):

```bash
GUIDELLM_API_KEY=... python3 bench/guidellm/runner.py run \
  --endpoint http://host.docker.internal:11434 --model MODEL \
  --contender marbor --out /path/to/results --run-kind "DRY RUN" --container
```

`--run-kind` is `DRY RUN`, `REHEARSAL` or `MEASURED` and is stamped on every
run, the manifest and the summary banner. `run` refuses to start if `slo.json`
no longer matches `slo.sha256`. Useful flags: `--shape`, `--profile` (repeat
to limit the matrix), `--requests-scale`, `--request-format`
(`/v1/chat/completions` default, `/v1/completions`), `--tokenizer`.

Run each contender with the same `--out` and a different `--contender`. The
first run of each contender is a warmup, recorded under `warmup/` and excluded
from the summary.

Summary table (contender x profile, per shape, plus `summary.csv`):

```bash
python3 bench/guidellm/runner.py summary --results /path/to/results
```

## Matrix

Shapes: 512 in / 256 out and 2048 in / 512 out, synthetic text. Profiles:
sweep (10 points), constant at 2, 4, 8 req/s, Poisson at 2, 4, 8 req/s,
concurrent 1, 4, 8, 16. Seed 20261031. Request counts are in `matrix.json`
(100 to 300 per profile, 200 per sweep point). `max_requests` applies per
benchmark, so a sweep runs that count at every point.

## Output layout

```
<out>/<contender>/manifest.jsonl              one record per run, failures included
<out>/<contender>/<shape>/<profile>.json      GuideLLM native JSON
<out>/<contender>/<shape>/<profile>.log       GuideLLM console log
<out>/<contender>/<shape>/<profile>.gpu.csv   nvidia-smi samples, when present
<out>/<contender>/crosscheck/                 vllm bench serve results (--cross-check)
<out>/env/<contender>/                        environment.json, commands.txt,
                                              matrix.json, slo.json, slo.sha256,
                                              nvidia-smi snapshot
<out>/summary.md, summary.csv
```

`env` captures the environment bundle on its own:

```bash
python3 bench/guidellm/runner.py env --endpoint URL --model M \
  --contender C --out DIR --run-kind MEASURED --note driver=... --note cuda=...
```

## Cross-check

`--cross-check` runs `vllm bench serve` once per shape (random dataset,
concurrency 8, 200 prompts, same seed) after that shape's matrix. It is
recorded as skipped, with the reason, if `vllm` is not on PATH
(`--vllm-cmd` overrides the command).

## SLO attainment

`slo_attain` is the share of attempted requests (errors count as misses) whose
TTFT and mean inter-token latency are both within the `slo.json` targets for
the shape. `slo_pass` compares it with `min_attainment`.
