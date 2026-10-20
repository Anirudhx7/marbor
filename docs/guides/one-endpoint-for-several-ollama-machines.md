# One endpoint for several Ollama machines

Install Marbor on one host, register each Ollama machine as a node, and give your apps a single URL. Marbor speaks the Ollama API and the OpenAI-compatible `/v1` API on port 11434, so both Ollama-style clients and OpenAI SDKs keep working, and it sends each request to a machine that already has the model loaded. No more picking a box by hand.

## 1. Make each Ollama machine reachable

By default Ollama listens on localhost only. On every machine that should serve traffic, bind it to the network:

```bash
# one-off
OLLAMA_HOST=0.0.0.0:11434 ollama serve
```

For a systemd install, run `sudo systemctl edit ollama` and add:

```ini
[Service]
Environment="OLLAMA_HOST=0.0.0.0:11434"
```

then `sudo systemctl restart ollama`. Check it from another machine: `curl http://10.0.1.11:11434/api/ps` should return JSON (Marbor polls this same endpoint for load and warm models).

Put the machines on a trusted network or firewall port 11434 to the Marbor host: Ollama itself has no authentication.

## 2. Install Marbor

On a separate host (or on one of the Ollama machines - then Marbor's proxy needs a different port than Ollama, since both default to 11434):

```bash
curl -fsSL raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | SERVICE=1 sh
```

The installer scans the local network for Ollama and offers to add what it finds; type `skip` to add nodes by hand. Open `http://<marbor-host>:8080`, sign in as `admin` / `admin`, and set a new password. Then:

```bash
marbor login --server http://<marbor-host>:8080
```

## 3. Register the machines

```bash
marbor nodes add gpu-1 http://10.0.1.11:11434 --gpu-model "RTX 4090" --vram-total-mb 24576
marbor nodes add gpu-2 http://10.0.1.12:11434 --gpu-model "RTX 3090" --vram-total-mb 24576
marbor nodes
```

`ollama` is the default runtime, so no `--runtime` flag is needed. Both nodes should show `healthy`.

## 4. Use the single endpoint

```bash
marbor key create --name laptop-apps    # prints the key once

# Ollama API
curl http://<marbor-host>:11434/api/chat \
  -H "Authorization: Bearer sk-laptop-apps-..." \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"hello"}],"stream":false}'

# OpenAI-compatible API
curl http://<marbor-host>:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-laptop-apps-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.1:8b","messages":[{"role":"user","content":"hello"}]}'
```

See what the fleet holds and where each model is warm:

```bash
marbor models fleet
```

## Ollama-only conveniences

Marbor does a few things for Ollama nodes that it does not yet do for other runtimes:

- **Keep-warm.** Hold priority models in VRAM between requests so the next call skips the cold load: set it in **Settings > Global Warmup** or per node on the **Warmup** page. Only models already pulled on that node can be added.
- **Pull and check fit from one place.** `marbor models pull gpu-1 llama3.1:8b` downloads a model to a chosen node, and `marbor fit --node gpu-1` tells you before you pull whether a model fits that node's VRAM and disk.

## What to set up next

- A key per app or teammate: [Per-key quotas and rate limits](per-key-quotas-self-hosted-llm.html)
- What happens when every machine is busy: [When every GPU is busy](when-every-gpu-is-busy.html)
- Put Open WebUI on top: [Open WebUI integration](../integrations/open-webui.html)
- Mix in vLLM boxes: [Load balance multiple vLLM servers](load-balance-vllm-servers.html)
- Reboot a machine safely: [Drain a GPU node](drain-gpu-node-without-downtime.html)
- Metrics and alerts: [Monitor the fleet](monitor-llm-fleet-prometheus-grafana.html)
