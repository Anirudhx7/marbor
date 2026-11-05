# Ollama is great. Until you have eight GPUs.

One endpoint, no per-box SSH loops, and no Kubernetes.

If you run Ollama on multiple machines, every box quickly becomes its own snowflake. You SSH into individual hosts to check `nvidia-smi`, remember which model was pulled where, set up blind reverse proxies with nginx, and write ad-hoc bash loops whenever a model updates. Ollama runs models cleanly; managing multiple instances as unified infrastructure is where operators hit friction.

Marbor sits in front of your Ollama instances and exposes a single endpoint on port 11434. You can pull, unload, or delete models, restart runtimes, and drain nodes for maintenance from one dashboard, CLI, or API - without SSHing into individual hosts. Behind the scenes, requests route to whichever node already has the model warm in VRAM.

## Quick start for Ollama fleets

### 1. Run Marbor with auto-discovery (systemd service)

Run this on your gateway or control host:

```bash
curl -fsSL raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | SERVICE=1 sh
```

The installer scans your local subnet for running Ollama instances on port 11434, prompts you to select discovered nodes, and starts Marbor as a managed systemd service that survives reboots.

### 2. Connect Open WebUI in one line

If you use Open WebUI, point it at Marbor instead of a single Ollama box:

```bash
OPENAI_BASE_URL=http://<marbor-host>:11434/v1
```

All existing models across your entire fleet become immediately visible and callable through Open WebUI.

### Alternative: Docker deployment

```bash
docker run -d --restart=unless-stopped \
  -p 11434:11434 -p 8080:8080 \
  -v marbor-data:/data \
  ghcr.io/anirudhx7/marbor:latest
```

## Three operational outcomes shipped today

1. **One endpoint across your fleet**: Exposes both the native Ollama API (`/api/*`) and the OpenAI-compatible API (`/v1/*`) on port 11434. Apps and SDKs send requests to one address, and Marbor sends each request to a node that already holds the model in memory.
2. **Remote operations without SSH**: Pull, unload, and delete models across nodes via the admin dashboard or CLI (`marbor models pull <model> --node <node>`). Gracefully drain a node (`marbor nodes drain <node>`) before system updates so in-flight requests finish without errors.
3. **Fleet-wide visibility**: View live per-model replicas, active requests, queue depth, and time-to-first-token (TTFT) across every host. Pre-flight disk fit checks hard-block pulls (HTTP 507) before dispatching to a node without enough disk space. Per-key rate limits and token quotas enforce usage limits (HTTP 429).

## Roadmap

- Rolling prefix-hash affinity for long multi-turn sessions (redesign in progress).
- Automatic `keep_alive` parameter injection for backends that support idle unloading.

## When NOT to use Marbor

- **Single-node localhost setup**: If you run Ollama on one laptop or workstation, standard Ollama is already sufficient. Marbor is designed for fleets with 2 or more nodes.
- **100+ GPU Kubernetes estates**: If you already maintain a dedicated Kubernetes platform team with custom CRDs and GPU operators, use a Kubernetes-native orchestration framework.
- **Cloud-only teams**: If your workloads run exclusively on hosted API providers (OpenAI, Anthropic) without local GPU hardware, you do not need an on-prem control plane.

## Getting started

- Step-by-step tutorial: [One endpoint for several Ollama machines](guides/one-endpoint-for-several-ollama-machines.md)
- Connect Open WebUI: [Open WebUI integration guide](integrations/open-webui.md)
- GitHub repository: [Anirudhx7/marbor](https://github.com/Anirudhx7/marbor)
