# Use several Macs as one OpenAI-compatible endpoint with MLX

Run `mlx_lm.server` on each Apple Silicon Mac, register each Mac in Marbor as an `mlx` node, and point your apps at one `/v1` URL. Marbor routes each request by model name to a Mac that serves it and watches each machine's health, so a rack of Mac Studios behaves like one inference endpoint instead of a list of hostnames.

## 1. Start the MLX server on each Mac

```bash
pip install mlx-lm
mlx_lm.server --model mlx-community/Llama-3.1-8B-Instruct-4bit \
  --host 0.0.0.0 --port 8081
```

Use a port other than 8080 (Marbor's admin port) and 11434 (its proxy port) if Marbor runs on the same Mac. Check from another machine:

```bash
curl http://10.0.1.40:8081/v1/models
```

The server has no `/health` route; Marbor probes `/v1/models` for MLX nodes. Macs can sleep, so on a headless Mac Studio disable sleep (`sudo pmset -a sleep 0`) before relying on it.

## 2. Install Marbor on one machine

Any Mac or Linux host that can reach the Macs works. On macOS:

```bash
curl -fsSL raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | sh
xattr -d com.apple.quarantine "$(command -v marbor)" 2>/dev/null
marbor
```

Marbor binaries are not yet Apple-notarized, so clear the quarantine flag once. The `SERVICE=1` installer mode only installs a systemd service on Linux; on macOS it falls back to a plain background process, so for a Mac control plane run `marbor` under your own `launchd` job if it must survive reboots.

Open `http://<marbor-host>:8080`, sign in as `admin` / `admin`, set a new password, then:

```bash
marbor login --server http://<marbor-host>:8080
```

## 3. Register each Mac

```bash
marbor nodes add mac-studio-1 http://10.0.1.40:8081 --runtime mlx --gpu-model "M2 Ultra" --vram-total-mb 196608
marbor nodes add mac-studio-2 http://10.0.1.41:8081 --runtime mlx --gpu-model "M2 Ultra" --vram-total-mb 196608
marbor nodes
```

`--vram-total-mb` is the unified memory you want to treat as usable for models; Marbor uses it for fit checks. Both nodes should be `healthy` with runtime `mlx`.

## 4. Send requests

```bash
marbor key create --name app

curl http://<marbor-host>:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-app-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"mlx-community/Llama-3.1-8B-Instruct-4bit","messages":[{"role":"user","content":"hello"}]}'
```

`marbor models fleet` shows each model and which Macs serve it. MLX nodes are reached on `/v1/*` only.

## MLX notes

- **Keep-warm is not available for MLX** (Ollama nodes only). The model stays loaded while `mlx_lm.server` runs.
- **Fit answers use the largest single memory pool.** `marbor fit <hf-owner/repo> --node mac-studio-1` tells you whether a Hugging Face safetensors repo fits before you download it.
- **One model per server process.** Run additional `mlx_lm.server` processes on other ports for more models and register each as its own node.

## What to set up next

- Quotas and a key per user: [Per-key quotas](per-key-quotas-self-hosted-llm.html)
- Busy or offline Macs: [When every GPU is busy](when-every-gpu-is-busy.html)
- Mix with Linux GPU boxes: [vLLM](load-balance-vllm-servers.html), [Ollama](one-endpoint-for-several-ollama-machines.html)
- Metrics: [Monitor the fleet](monitor-llm-fleet-prometheus-grafana.html)
- Updating a Mac: [Drain a node](drain-gpu-node-without-downtime.html)
