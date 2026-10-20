# Load balance multiple vLLM servers behind one OpenAI-compatible endpoint

Run `vllm serve` on each GPU host, register each one with Marbor as a `vllm` node, and point your clients at Marbor's `/v1` URL. Marbor is a single binary that does the load balancing: it sends each request to a node that already has the requested model loaded, falls back to the node with the fewest active connections, and keeps serving while a node is down. No nginx or HAProxy config, no Kubernetes.

This guide ends with working requests going through one endpoint to two vLLM servers. It takes about ten minutes.

## 1. Start vLLM on each GPU host

On every GPU host, serve one model per vLLM process and bind to a reachable address. Pick a port that does not collide with Marbor (8080 admin, 11434 proxy):

```bash
pip install vllm
vllm serve meta-llama/Llama-3.1-8B-Instruct --host 0.0.0.0 --port 8000
```

Or with Docker:

```bash
docker run -d --gpus all --ipc=host -p 8000:8000 \
  -v ~/.cache/huggingface:/root/.cache/huggingface \
  vllm/vllm-openai:latest --model meta-llama/Llama-3.1-8B-Instruct
```

Check it from another machine: `curl http://10.0.1.20:8000/health` should return HTTP 200 (this is the same endpoint Marbor uses for its health probe).

vLLM serves one model per process. To serve a second model on the same host, start a second vLLM on another port and register it as its own node (below).

## 2. Install Marbor on one machine

Pick any host that can reach your GPU hosts. Install it as a service:

```bash
curl -fsSL raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | SERVICE=1 sh
```

The installer scans your network for backends and asks which to add. Type `skip` to add nodes yourself in the next step. Prefer Docker? `git clone https://github.com/Anirudhx7/marbor && cd marbor && docker compose up -d`.

Open `http://<marbor-host>:8080`, sign in as `admin` / `admin`, and set a new password when prompted (the first-login change cannot be skipped). Then log the CLI in once:

```bash
marbor login --server http://<marbor-host>:8080
```

## 3. Register each vLLM server

```bash
marbor nodes add vllm-a http://10.0.1.20:8000 --runtime vllm --gpu-model "RTX 4090" --vram-total-mb 24576
marbor nodes add vllm-b http://10.0.1.21:8000 --runtime vllm --vram-total-mb 24576
marbor nodes
```

Within a few seconds both nodes show `healthy` with runtime `vllm`. Marbor polls each node's `/health` and `/v1/models` to learn which model it serves.

## 4. Point clients at one endpoint

Create a key (it prints once, store it):

```bash
marbor key create --name app
```

Then use any OpenAI client against Marbor's proxy port:

```bash
curl http://<marbor-host>:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-app-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"meta-llama/Llama-3.1-8B-Instruct","messages":[{"role":"user","content":"hello"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://<marbor-host>:11434/v1", api_key="sk-app-...")
print(client.chat.completions.create(
    model="meta-llama/Llama-3.1-8B-Instruct",
    messages=[{"role": "user", "content": "hello"}],
).choices[0].message.content)
```

`GET /v1/models` through Marbor lists the models of the whole fleet, and `marbor models fleet` shows which nodes hold each one.

## How requests are placed

The default strategy is `warm-first`: if the model is loaded on a healthy node, the request goes to the warm node with the fewest active connections. If not, Marbor falls back to least-connections. It is not round-robin, so do not expect an even request split between nodes. Read the current strategy with `marbor routing strategy get`, and see the [production guide](../PRODUCTION.html) for running Marbor as a service.

vLLM nodes are reached on `/v1/*` only. Ollama-style `/api/*` paths go to Ollama nodes, so a mixed fleet works with OpenAI clients unchanged.

## What to set up next

- Give each team or app its own key with limits: [Per-key quotas and rate limits](per-key-quotas-self-hosted-llm.html)
- What a client sees when every node is busy or down: [When every GPU is busy](when-every-gpu-is-busy.html)
- Graphs and alerts: [Monitor a self-hosted LLM fleet with Prometheus](monitor-llm-fleet-prometheus-grafana.html)
- Maintenance without dropping requests: [Drain a GPU node](drain-gpu-node-without-downtime.html)
- Mixed with other runtimes: [Ollama](one-endpoint-for-several-ollama-machines.html), [llama.cpp](several-llama-cpp-servers-one-endpoint.html), [Macs with MLX](mac-fleet-mlx-one-endpoint.html)
