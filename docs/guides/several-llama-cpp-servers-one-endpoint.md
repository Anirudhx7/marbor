# Run several llama.cpp servers behind one OpenAI-compatible endpoint

Start `llama-server` on each machine, register each as a `llamacpp` node in Marbor, and send OpenAI-style requests to Marbor's `/v1` URL. Marbor picks a server that has the requested model, checks health on each node, and keeps a fleet of GGUF servers behind one address.

## 1. Start llama-server on each machine

`llama-server` serves one GGUF model per process. Bind to the network and avoid ports 8080 and 11434 on the Marbor host:

```bash
llama-server -m ./Qwen2.5-7B-Instruct-Q4_K_M.gguf \
  --host 0.0.0.0 --port 8081 -ngl 99 -c 8192
```

`-ngl 99` offloads all layers to the GPU; lower it if the model does not fit. Check it from another machine: `curl http://10.0.1.30:8081/health` returns HTTP 200 once the model is loaded (Marbor uses `/health` and `/v1/models`).

Note the model name the server reports, because clients must use that exact name through Marbor:

```bash
curl http://10.0.1.30:8081/v1/models
```

## 2. Install Marbor

```bash
curl -fsSL raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | SERVICE=1 sh
```

Type `skip` at the discovery prompt to add nodes yourself. Sign in at `http://<marbor-host>:8080` as `admin` / `admin`, set a new password, then:

```bash
marbor login --server http://<marbor-host>:8080
```

## 3. Register each server

```bash
marbor nodes add llama-1 http://10.0.1.30:8081 --runtime llamacpp --gpu-model "RTX 4090" --vram-total-mb 24576
marbor nodes add llama-2 http://10.0.1.31:8081 --runtime llamacpp --vram-total-mb 24576
marbor nodes
```

Both should show `healthy` with runtime `llamacpp`. `marbor models fleet` lists the model each server reports and which nodes hold it.

## 4. Call the single endpoint

```bash
marbor key create --name app

curl http://<marbor-host>:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-app-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen2.5-7B-Instruct-Q4_K_M.gguf","messages":[{"role":"user","content":"hello"}]}'
```

Use whatever model id `/v1/models` printed in step 1. llama.cpp nodes are reached on `/v1/*` only. If you would rather have clients ask for a friendly name, define a model alias so `qwen-7b` resolves to that id:

```bash
marbor models alias set qwen-7b Qwen2.5-7B-Instruct-Q4_K_M.gguf
```

## Limits to know about

- **No keep-warm for llama.cpp yet.** Marbor's keep-warm feature is Ollama only; a `llama-server` keeps its model loaded as long as the process runs, so this rarely matters.
- **One model per server.** Run one `llama-server` per model and register each separately. Marbor routes by the model name in the request.
- **Check fit before you download.** `marbor fit hf-owner/some-model-GGUF --node llama-1 --ctx 8192` says whether a Hugging Face GGUF repo fits that node's VRAM and which quantization to pick. It is read-only.

## What to set up next

- Keys with quotas and model allow-lists: [Per-key quotas](per-key-quotas-self-hosted-llm.html)
- What clients see when all servers are busy: [When every GPU is busy](when-every-gpu-is-busy.html)
- Add vLLM or Ollama servers to the same endpoint: [vLLM](load-balance-vllm-servers.html), [Ollama](one-endpoint-for-several-ollama-machines.html)
- Metrics: [Monitor the fleet](monitor-llm-fleet-prometheus-grafana.html)
- Updating the llama.cpp build on a node: [Drain a GPU node](drain-gpu-node-without-downtime.html)
