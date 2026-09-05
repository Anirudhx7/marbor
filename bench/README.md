# TTFT Benchmark - Warm vs Cold Routing

**Time-To-First-Token (TTFT)** is the wall-clock milliseconds from when the
client sends a request to when the first byte of the streaming response arrives.
It is the most user-visible latency number for interactive LLM use.

## What this benchmark measures

`bench/ttft.go` sends N streaming `/api/generate` (or `/api/chat`) requests to
a target URL and records TTFT for each.  You run it twice - once against a cold
path, once against a warm path - then compare the numbers.

**Want the fast path?** Skip straight to [Guided scripts](#guided-scripts---preflight-check--automated-cold-loop) below -
`bench/preflight.sh` (checks your setup before you waste time) and
`bench/cold-loop.sh` (real n≥10 cold samples, copy-paste ready).

## The two scenarios

### Scenario A - Cold (baseline)

A request lands on an Ollama node that does **not** have the model loaded in
VRAM.  Ollama must read the model weights from disk before generating the first
token.  On a consumer GPU with a 7B model this typically takes 10-40 seconds
depending on disk speed and model size.

How to reproduce: send the first-ever request to a fresh Ollama node for a
model it has never served (or restart Ollama and wait for it to evict VRAM).

### Scenario B - Warm via marbor (target)

marbor polls every node's `/api/ps` endpoint every 2 seconds to track
which models are currently loaded in VRAM.  When a request arrives, the router
selects a node that already has the model warm.  The node skips the disk-load
step and starts generating immediately.

The headline marketing claim is: **warm-routed TTFT is roughly the GPU's pure
generation latency, not GPU generation latency plus model load time.**

## Honesty caveat - mock servers cannot demonstrate this

The `cmd/mocknode` servers use a fixed `LATENCY_MS` sleep to simulate
response delay.  They respond instantly regardless of WARM_MODELS state.
**Running this benchmark against the mock nodes or the demo stack will
produce sub-200 ms numbers that mean nothing about real cold-load savings.**

The headline number MUST be captured on real Ollama running on real GPU
hardware where model-load delay is genuine (tens of seconds for large models).
Do not publish numbers from mock runs.

## Build the benchmark tool

From the repo root (requires Go 1.22+ or Docker):

```bash
# With a local Go toolchain
go build -o bench/ttft ./bench

# Without a local Go toolchain (Docker only)
docker run --rm -v "${PWD}:/app" -w /app -e GOFLAGS=-buildvcs=false \
  golang:1.22 go build -o bench/ttft ./bench
```

## How to reproduce on real hardware (2-node setup)

**Prerequisites:**
- Two machines (or VMs) each running `ollama serve`
- marbor configured with both nodes added via the dashboard's **GPU Nodes** page (or `--seed-node`)
- The model you want to test pulled on both nodes (`ollama pull llama3.2:3b`)
- A client API key issued from your running marbor instance (dashboard's API Keys page, or the admin API) - not a config file, marbor is fully DB-based

**Step 1 - Record cold TTFT (Scenario A)**

Point the tool directly at one Ollama node with the model *not* loaded in VRAM.
Restart Ollama to guarantee a cold state, then immediately run:

```bash
./bench/ttft \
  --url http://<node-a-ip>:11434 \
  --model llama3.2:3b \
  --n 3
```

The first request will be slow (model load + generation).  Record p50.

**Step 2 - Warm the model on node-a via the marbor**

Send one request through marbor to node-a so it loads the model:

```bash
./bench/ttft \
  --url http://<marbor-ip>:11434 \
  --model llama3.2:3b \
  --n 1 \
  --api-key <your-key>
```

Wait a few seconds for `/api/ps` to confirm the model is warm (visible in the
dashboard's GPU Nodes page, or via `curl http://<marbor-ip>:8080/admin/v1/nodes`
with an admin session cookie).

**Step 3 - Record warm TTFT via marbor (Scenario B)**

```bash
./bench/ttft \
  --url http://<marbor-ip>:11434 \
  --model llama3.2:3b \
  --n 10 \
  --api-key <your-key>
```

Record p50 and p95.

**Step 4 - Verify cold baseline again**

Restart Ollama on node-a to evict VRAM and repeat Step 1 with `--n 10`.  The
first request will be cold; subsequent ones will be warm (Ollama keeps the model
loaded between requests by default).  Use only request 1 as the cold sample, or
restart Ollama before each request if you want N independent cold samples.

## Guided scripts - preflight check + automated cold-loop

The manual steps above work fine by hand, but two scripts remove the tedious
and error-prone parts: checking your setup is actually ready, and evicting the
model before every single cold sample (a real n≥10 cold baseline, not just a
cold first request).

**Prerequisites - these scripts don't install or start anything for you:**
- marbor already running (admin API and proxy reachable - defaults
  `:8080`/`:11434`)
- At least one GPU backend node (Ollama/vLLM/TGI/llama.cpp/MLX) added and
  healthy, with the model you want to test already pulled on it
- `bash`, `curl`, and `python3` on PATH
- `bench/ttft` built (see "Build the benchmark tool" above)
- An admin account (default `admin`/`admin`) and a client API key from that
  marbor instance
- Real GPU hardware, not the demo/mock stack - see the honesty caveat above

### `bench/preflight.sh` - run this first

Fails loudly, with a clear reason, before you waste time on a broken setup.
Checks: admin login actually works, the target node exists and is healthy,
the model is loaded (or at least pulled), and - if you provide the sizes -
that the model safely fits under 80% of the node's VRAM.

marbor is fully DB-based (`marbor.db`) - there's no config file to read a
token from. Admin auth is the same session login the dashboard uses: an
admin-role account's username and password (`admin`/`admin` in demo mode;
otherwise whatever account you created via the dashboard's user setup).

**Single-node setup - just set `MODEL` and go:**

```bash
MODEL="llama3.2:3b-q4_k_m" ./bench/preflight.sh
```

`ADMIN_USERNAME`/`ADMIN_PASSWORD` default to `admin`/`admin`, and `NODE_NAME`
is auto-detected when marbor has exactly one node. Only `MODEL` (the exact
tag you're about to benchmark) can't be guessed - the script won't silently
pick a model for you, since a wrong guess would produce misleading numbers.

**Full form** (multi-node marbor, non-default admin account, or the optional
VRAM-fit check):

```bash
MARBOR_URL="http://localhost:11434" \
ADMIN_URL="http://localhost:8080" \
ADMIN_USERNAME="admin" \
ADMIN_PASSWORD="<that account's password>" \
NODE_NAME="<exact node name from GET /admin/nodes>" \
MODEL="llama3.2:3b-q4_k_m" \
MODEL_SIZE_GB=2.0 \
NODE_VRAM_GB=24 \
./bench/preflight.sh
```

| Env var | Required? | What it's for |
|---|---|---|
| `MARBOR_URL` | no (defaults to `http://localhost:11434`) | marbor proxy base URL (the legacy `MESH_URL` spelling is still accepted as a fallback) |
| `ADMIN_URL` | no (defaults to `http://localhost:8080`) | marbor admin API base URL |
| `ADMIN_USERNAME` | no (defaults to `admin`) | an admin-role account's username |
| `ADMIN_PASSWORD` | no (defaults to `admin`) | that account's password |
| `NODE_NAME` | no (auto-detected if only one node exists) | exact node name, from `GET /admin/nodes` |
| `MODEL` | **yes** | exact model tag you're about to benchmark |
| `MODEL_SIZE_GB` | no | enables the automatic 80%-VRAM fit check |
| `NODE_VRAM_GB` | no | enables the automatic 80%-VRAM fit check |

The model-pulled check goes through the marbor's own
`GET /admin/nodes/{name}/models` API (via the node's marbor agent), so it works
the same way regardless of which runtime that node is running - Ollama, vLLM,
TGI, llama.cpp, or MLX. If the node has no marbor agent `models.list`
capability enabled, that one check is skipped with a manual reminder instead
of failing outright.

A clean run ends with:

```
=== [5/5] Preflight passed ===
Safe to proceed with bench/cold-loop.sh.
```

### `bench/cold-loop.sh` - real n≥10 cold samples

Evicts the model from VRAM, waits, fires one request, and repeats - so every
sample is a genuine cold load, not just the first one in a batch.

**Single-node setup - just set `MODEL` and `API_KEY`:**

```bash
MODEL="llama3.2:3b-q4_k_m" API_KEY="<a valid client API key>" ./bench/cold-loop.sh 10
```

Same defaults/auto-detection as `preflight.sh` above (`admin`/`admin`,
auto-detected `NODE_NAME`). `MODEL` and `API_KEY` are the two things that
can't be guessed for you.

**Full form** (multi-node marbor or non-default admin account):

```bash
MARBOR_URL="http://localhost:11434" \
ADMIN_URL="http://localhost:8080" \
ADMIN_USERNAME="admin" \
ADMIN_PASSWORD="<that account's password>" \
NODE_NAME="<exact node name from GET /admin/nodes>" \
MODEL="llama3.2:3b-q4_k_m" \
API_KEY="<a valid client API key>" \
./bench/cold-loop.sh 10
```

The trailing `10` is the sample count (n) - omit it to default to 10. Requires
`bench/ttft` already built (see "Build the benchmark tool" above).

| Env var | Required? | What it's for |
|---|---|---|
| `MARBOR_URL` | no (defaults to `http://localhost:11434`) | marbor proxy base URL (the legacy `MESH_URL` spelling is still accepted as a fallback) |
| `ADMIN_URL` | no (defaults to `http://localhost:8080`) | marbor admin API base URL |
| `ADMIN_USERNAME` | no (defaults to `admin`) | an admin-role account's username, used to log in before each eviction |
| `ADMIN_PASSWORD` | no (defaults to `admin`) | that account's password |
| `NODE_NAME` | no (auto-detected if only one node exists) | exact node name to evict from |
| `MODEL` | **yes** | exact model tag |
| `API_KEY` | **yes** | client API key used for the actual benchmark requests |

Output ends with one summary line plus a raw log you can double-check:

```
=== Aggregating 10 cold samples from cold_samples.log ===
n=10  p50=17325.0 ms  min=11466.0 ms  max=21902.4 ms
```

That `p50`/`min`/`max` line is what goes into the results table below as your
"Cold" row. Raw per-sample values are saved to `cold_samples.log` in your
current directory (overwritten on each run) if you need to inspect anything.

## Results table - fill in with your hardware

| Scenario | Hardware | Model | n | p50 TTFT | p95 TTFT |
|----------|----------|-------|---|----------|----------|
| A - Cold direct | _your GPU + disk_ | _model_ | _n_ | ___ ms | ___ ms |
| B - Warm via marbor | _your GPU_ | _model_ | _n_ | ___ ms | ___ ms |
| **Savings (A-B)** | | | | ___ ms | |

Fill this table in from a real run and paste the numbers into the README or a
GitHub Discussions post.  Do not publish fabricated or mock-derived numbers.

## Reference run (v0.13.1, real hardware)

Measured 2026-07-02 through a deployed marbor v0.13.1 routing to a single
consumer-GPU Ollama node.  Model: 8B Q4_K_M (~9.6 GB on disk; only ~3.3 GB of its
~10.6 GB runtime footprint fit in VRAM, so warm TTFT was partly CPU-bound - expect
tighter warm numbers on a GPU that fully fits the model).  Cold samples each
followed a real eviction of the model from VRAM, confirmed via `/api/ps`.

| Scenario | n | p50 TTFT | min | max |
|----------|---|----------|-----|-----|
| Cold via marbor | 3 | 17,325 ms | 11,466 ms | 18,128 ms |
| Warm via marbor (spaced 20 s apart) | 10 | 8,079 ms | 1,915 ms | 13,785 ms |
| Warm direct-to-node (control) | 5 | 8,595 ms | 1,921 ms | 15,842 ms |

Fastest warm sample via marbor: 401 ms.  The direct-to-node control shows the same
warm profile as via-marbor, i.e. marbor proxy overhead is negligible.

Measurement notes: back-to-back `--n 10` runs overlap with the previous request's
still-draining generation (the tool drains responses in a background goroutine),
which skews TTFT samples - space independent samples apart (e.g. one `--n 1` run
every 20 s) for clean warm numbers.

## Flags reference

| Flag | Default | Description |
|------|---------|-------------|
| `--url` | `http://localhost:11434` | Base URL of the target endpoint |
| `--model` | `llama3.2:3b` | Model to request |
| `--n` | `10` | Number of requests |
| `--api-key` | _(empty)_ | Bearer key (required for marbor, omit for direct Ollama) |
| `--endpoint` | `generate` | `generate` or `chat` |

---

# SQLite write-path load sweep (`bench/loadtest`)

**What this measures:** marbor's SQLite write path (`audit_log`, `request_log`,
`hourly_buckets`/`model_stats`) is already fully async - all three writes go through
bounded (5000-slot) buffered channels with drop-on-full, not the request goroutine
(`internal/audit/audit.go`, `internal/admin/admin.go`'s `logChan`/`statsChan`). Nobody has
measured what request rate that async design can actually absorb before it starts dropping
entries, or how the SQLite WAL file grows under sustained write pressure. `bench/loadtest`
sweeps request rate against the real marbor + real SQLite store and reports a latency/file-growth
curve - it does **not** compute a single "ceiling" number for you.

**Honesty caveat - what this tool measures vs. doesn't:** unlike `bench/ttft`, mock nodes
(`cmd/mocknode`) are appropriate here, since the target is write-path throughput to SQLite, not
inference latency fidelity - unlike the TTFT benchmark, mocked GPU latency doesn't invalidate
this measurement. What it does NOT give you is an inference-capacity number; keep write-path
capacity and inference-capacity claims separate.

**How to read the results - do not invent a threshold:**
- The tool prints a table of target/sent/completed/failed RPS and p50/p95/p99 latency per swept
  rate, plus `.db`/`-wal` file size deltas per step.
- It never issues `PRAGMA wal_checkpoint` during a run - that pragma actively forces a
  checkpoint rather than passively reading state, which would perturb the exact WAL-growth
  behavior under test. File sizes are the only signal used.
- **The real ceiling is not in this table.** Watch the marbor's own log output (stdout or wherever
  it's redirected) during the run for the queue-full drop lines:
  - `audit logger: queue full, dropped audit entry for request ...`
  - `async logger: queue full, dropped request log ...`
  - `async logger: stats queue full, dropped hourly/model-stat update for ...`

  The first swept rate at which any of these appears is the actual operational limit -
  report that rate, not a rounded-up guess.
- **If no drop line appears across the whole sweep**, that's a real, reportable result:
  write it up as *"no drop ceiling observed up to N req/s"*, where N is the highest rate
  tested - never present the highest tested rate as if it were a proven ceiling.
- **Check the `GENERATOR-SATURATED` flag** on every row before trusting it. If the tool's own
  sent RPS falls more than `--generator-slack-pct` (default 10%) short of the target RPS for a
  step, that step's numbers reflect the load generator's own limits, not the marbor's - don't cite
  a saturated-generator step as evidence of a marbor ceiling.

## Setup

1. Start one or more `cmd/mocknode` instances with a low `LATENCY_MS` and the target model in
   `WARM_MODELS` (so every request is warm - this test is not measuring cold-load time):
   ```bash
   RUNTIME=ollama NODE_NAME=loadtest-node PORT=21434 LATENCY_MS=20 \
     WARM_MODELS=llama3.2:3b ALL_MODELS=llama3.2:3b go run ./cmd/mocknode
   ```
2. Register that node with the marbor (dashboard's GPU Nodes page, or `POST /admin/nodes` -
   same auth as `bench/preflight.sh` uses, session login via `/admin/login`).
3. Build the tool:
   ```bash
   go build -o bench/loadtest ./bench/loadtest
   ```
4. Run the sweep, pointing `--db` at the marbor's actual database file - the same path the marbor
   itself was started with (its own `--db` flag or `MARBOR_DB_PATH` env var, default `marbor.db` in
   its working directory; `bench/loadtest`'s own `--db` flag is a separate, read-only pointer to
   that same file for passive size sampling, not a shared setting with the marbor process):
   ```bash
   ./bench/loadtest --url http://localhost:11434 --model llama3.2:3b \
     --api-key <your-key> --db marbor.db --rates 5,10,20,40,80,160 --step-duration 30s
   ```
5. **Establish the single-node baseline first** - one marbor process, one mock backend. This sweep
   is about the SQLite write path itself, which doesn't necessarily scale with inference node count,
   so treating "N nodes" as the primary axis would mislabel what's being measured. Only after the
   single-node baseline is recorded, optionally repeat with more registered mock nodes (e.g. 20)
   as a **secondary** check for whether concurrent request sources from more nodes changes the
   write-path ceiling - report that separately, not folded into the primary claim.
6. Re-run once to confirm the drop point (or lack of one) is reproducible before writing a number
   into `docs/PRODUCTION.md`.

## Flags reference

| Flag | Default | Description |
|------|---------|-------------|
| `--url` | `http://localhost:11434` | Base URL of the marbor proxy |
| `--model` | `llama3.2:3b` | Model to request (must be warm on the target node) |
| `--api-key` | _(empty)_ | Bearer API key for the marbor |
| `--db` | `marbor.db` | Path to the marbor's SQLite database, for passive `.db`/`-wal` size sampling |
| `--rates` | `5,10,20,40,80` | Comma-separated ascending list of target req/s to sweep |
| `--step-duration` | `20s` | How long to sustain each rate before moving to the next |
| `--endpoint` | `chat` | `generate` or `chat` |
| `--generator-slack-pct` | `10` | Max allowed shortfall (%) between target and actual-sent RPS before a step is flagged generator-saturated |
| `--max-inflight` | `1000` | Cap on concurrent in-flight requests - once hit, new requests wait for a slot instead of spawning unbounded goroutines/sockets, so a step the generator can't sustain shows up as a sent-RPS shortfall instead of exhausting the generator's own resources |
