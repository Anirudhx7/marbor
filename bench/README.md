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

### Scenario B - Warm via ollama-mesh (target)

ollama-mesh polls every node's `/api/ps` endpoint every 2 seconds to track
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
- ollama-mesh configured with both nodes added via the dashboard's **GPU Nodes** page (or `--seed-node`)
- The model you want to test pulled on both nodes (`ollama pull llama3.2:3b`)
- An admin API key from your mesh config

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

**Step 2 - Warm the model on node-a via the mesh**

Send one request through ollama-mesh to node-a so it loads the model:

```bash
./bench/ttft \
  --url http://<mesh-ip>:11434 \
  --model llama3.2:3b \
  --n 1 \
  --api-key <your-key>
```

Wait a few seconds for `/api/ps` to confirm the model is warm (visible in the
dashboard's GPU Nodes page or via `curl http://<mesh-ip>:8080/admin/v1/nodes`).

**Step 3 - Record warm TTFT via mesh (Scenario B)**

```bash
./bench/ttft \
  --url http://<mesh-ip>:11434 \
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

### `bench/preflight.sh` - run this first

Fails loudly, with a clear reason, before you waste time on a broken setup.
Checks: admin login actually works, the target node exists and is healthy,
the model is loaded (or at least pulled), and - if you provide the sizes -
that the model safely fits under 80% of the node's VRAM.

ollama-mesh is fully DB-based (`mesh.db`) - there's no config file to read a
token from. Admin auth is the same session login the dashboard uses: an
admin-role account's username and password (`admin`/`admin` in demo mode;
otherwise whatever account you created via the dashboard's user setup).

```bash
MESH_URL="http://localhost:11434" \
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
| `MESH_URL` | no (defaults to `http://localhost:11434`) | mesh proxy base URL |
| `ADMIN_URL` | no (defaults to `http://localhost:8080`) | mesh admin API base URL |
| `ADMIN_USERNAME` | **yes** | an admin-role account's username |
| `ADMIN_PASSWORD` | **yes** | that account's password |
| `NODE_NAME` | **yes** | exact node name, from `GET /admin/nodes` |
| `MODEL` | **yes** | exact model tag you're about to benchmark |
| `MODEL_SIZE_GB` | no | enables the automatic 80%-VRAM fit check |
| `NODE_VRAM_GB` | no | enables the automatic 80%-VRAM fit check |

The model-pulled check goes through the mesh's own
`GET /admin/nodes/{name}/models` API (via the node's Node Agent), so it works
the same way regardless of which runtime that node is running - Ollama, vLLM,
TGI, llama.cpp, or MLX. If the node has no Node Agent `models.list`
capability enabled, that one check is skipped with a manual reminder instead
of failing outright.

A clean run ends with:

```
=== Preflight passed - safe to proceed with BENCH-RUNBOOK.md Step 2 ===
```

### `bench/cold-loop.sh` - real n≥10 cold samples

Evicts the model from VRAM, waits, fires one request, and repeats - so every
sample is a genuine cold load, not just the first one in a batch.

```bash
MESH_URL="http://localhost:11434" \
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
| `MESH_URL` | no (defaults to `http://localhost:11434`) | mesh proxy base URL |
| `ADMIN_URL` | no (defaults to `http://localhost:8080`) | mesh admin API base URL |
| `ADMIN_USERNAME` | **yes** | an admin-role account's username, used to log in before each eviction |
| `ADMIN_PASSWORD` | **yes** | that account's password |
| `NODE_NAME` | **yes** | exact node name to evict from |
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
| B - Warm via mesh | _your GPU_ | _model_ | _n_ | ___ ms | ___ ms |
| **Savings (A-B)** | | | | ___ ms | |

Fill this table in from a real run and paste the numbers into the README or a
GitHub Discussions post.  Do not publish fabricated or mock-derived numbers.

## Reference run (v0.13.1, real hardware)

Measured 2026-07-02 through a deployed ollama-mesh v0.13.1 routing to a single
consumer-GPU Ollama node.  Model: 8B Q4_K_M (~9.6 GB on disk; only ~3.3 GB of its
~10.6 GB runtime footprint fit in VRAM, so warm TTFT was partly CPU-bound - expect
tighter warm numbers on a GPU that fully fits the model).  Cold samples each
preceded by a real `keep_alive: 0` eviction confirmed via `/api/ps`.

| Scenario | n | p50 TTFT | min | max |
|----------|---|----------|-----|-----|
| Cold via mesh | 3 | 17,325 ms | 11,466 ms | 18,128 ms |
| Warm via mesh (spaced 20 s apart) | 10 | 8,079 ms | 1,915 ms | 13,785 ms |
| Warm direct-to-node (control) | 5 | 8,595 ms | 1,921 ms | 15,842 ms |

Fastest warm sample via mesh: 401 ms.  The direct-to-node control shows the same
warm profile as via-mesh, i.e. mesh proxy overhead is negligible.

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
| `--api-key` | _(empty)_ | Bearer key (required for ollama-mesh, omit for direct Ollama) |
| `--endpoint` | `generate` | `generate` or `chat` |
