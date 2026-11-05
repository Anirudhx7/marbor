# Your Macs as one AI endpoint

Run an Apple Silicon cluster with unified memory behind one OpenAI-compatible endpoint. No per-Mac SSH loops, and no Kubernetes.

Apple Silicon Macs (Mac Studio, Mac Pro, Mac mini) provide massive unified memory at accessible hardware costs, making them popular for running large models locally with MLX (`mlx_lm.server`). However, running three or four Macs together usually means SSHing into each machine separately, managing ports by hand, and having no shared endpoint or fleet-level view of available capacity.

Marbor runs natively on macOS (`darwin-arm64`) as a single static binary. It pools multiple Mac nodes behind one OpenAI-compatible endpoint (`/v1/chat/completions`), provides model fit guidance based on unified memory capacity, and lets you manage nodes centrally without touching SSH.

## Quick start for Apple Silicon fleets

### 1. Install Marbor on macOS

Download and run Marbor on your primary Mac or gateway:

```bash
curl -fsSL raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | sh
```

If macOS Gatekeeper quarantines the downloaded binary on first run, clear the quarantine attribute:

```bash
xattr -d com.apple.quarantine /usr/local/bin/marbor
```

Start the gateway:

```bash
marbor
```

The admin dashboard is immediately accessible at `http://localhost:8080`.

### 2. Check model fit before loading

Use the built-in catalog advisor to see whether a model fits your Mac's unified memory:

```bash
marbor fit --model qwen2.5:32b
```

Marbor analyzes unified memory capacity to indicate green (fits comfortably), yellow (tight fit requiring context limits), or red (exceeds capacity), avoiding false eviction alerts when idle models are resident.

### 3. Register your MLX nodes

On each worker Mac, start `mlx_lm.server`:

```bash
python -m mlx_lm.server --model mlx-community/Qwen2.5-32B-Instruct-4bit --port 8080
```

Register the Mac as a node in Marbor:

```bash
marbor nodes add mac-studio-1 http://192.168.1.50:8080 \
  --runtime mlx \
  --gpu-model "Apple M3 Max" \
  --vram-total-mb 65536
```

### 4. Send requests through the single endpoint

```bash
curl http://<marbor-host>:11434/v1/chat/completions \
  -H "Authorization: Bearer <your-marbor-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"mlx-community/Qwen2.5-32B-Instruct-4bit","messages":[{"role":"user","content":"Hello"}]}'
```

## Three operational outcomes shipped today

1. **Unified OpenAI endpoint**: Connect any OpenAI-compatible tool, coding assistant (Continue, Cursor), or chat UI to `http://<marbor-host>:11434/v1`. Marbor routes requests to whichever Mac hosts the requested model, translating endpoints and validating model availability.
2. **Memory-aware capacity guidance**: Apple Silicon shares unified memory between CPU and GPU. Marbor accounts for host RAM telemetry alongside model weight estimates to give operators realistic fit advice before dispatching workloads.
3. **Multi-host fleet management**: Manage all Mac nodes from the web dashboard or CLI. Drain a Mac before rebooting or updating macOS, inspect real-time response latency, and monitor error rates across nodes.

## Hardware verification status

> [!NOTE]
> MLX runtime compatibility and Apple Silicon telemetry collectors (`system_profiler SPDisplaysDataType`) are implemented against public specifications and unit-tested in CI. Validated performance benchmark checkmarks remain pending until multi-host physical Apple Silicon bounty hardware testing is completed.

## Roadmap

- Official Homebrew formula packaging (`brew install marbor`).
- Request schema adaptation for runtime-specific parameters (e.g. `top_k`, `logit_bias`).
- Benchmarking throughput and latency across multi-Mac Studio clusters on physical hardware.

## When NOT to use Marbor

- **Single Mac setup**: If you run models on a single Mac laptop or desktop, running `mlx_lm.server` directly is simpler.
- **100+ GPU Kubernetes environments**: Enterprise Linux clusters with dedicated Kubernetes infrastructure should use K8s-native orchestration.
- **Cloud-only inference**: If you do not self-host models on local hardware, a local control plane is unnecessary.

## Next steps

- Step-by-step tutorial: [Several Macs as one MLX endpoint](guides/mac-fleet-mlx-one-endpoint.md)
- Node registration reference: [GPU node registration](deploy/gpu-node-registration.md)
- GitHub repository: [Anirudhx7/marbor](https://github.com/Anirudhx7/marbor)
