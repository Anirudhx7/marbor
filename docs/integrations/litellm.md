# LiteLLM

[LiteLLM](https://litellm.ai) is a popular and powerful API gateway for provider abstraction, enterprise authentication, and user-level rate limiting. 

Rather than a competitor, **ollama-mesh operates as a complementary layer alongside LiteLLM**. They can be integrated in two distinct topologies depending on your architecture:

- **Option A: Upstream Gateway Setup (LiteLLM &rarr; ollama-mesh)**: LiteLLM sits in front of your applications as the entry point and routes local model requests to ollama-mesh.
- **Option B: Cloud Fallback Gateway Setup (ollama-mesh &rarr; LiteLLM)**: ollama-mesh routes cloud fallback traffic (when all local GPUs are saturated or down) through a central LiteLLM instance instead of hitting cloud endpoints directly.

---

## Prerequisites

- LiteLLM running (Docker or bare-metal)
- ollama-mesh running and reachable from the LiteLLM host
- An API key from the ollama-mesh admin dashboard (`http://<mesh-host>:8080`)

---

## Option A - Upstream Gateway Setup (LiteLLM &rarr; ollama-mesh)

This is the recommended setup when you want to use LiteLLM's enterprise controls (such as SSO, virtual keys, and multi-cloud routing) while leveraging ollama-mesh's warm-first GPU load balancing and local VRAM scheduling.

```
Applications ────> LiteLLM (Gateway) ────> ollama-mesh (Scheduler) ────> GPU Nodes
```

### LiteLLM Configuration
Add ollama-mesh as a custom provider in LiteLLM's `config.yaml` using the OpenAI-compatible endpoint:

```yaml
model_list:
  - model_name: llama3.2
    litellm_params:
      model: openai/llama3.2
      api_base: http://<mesh-host>:11434/v1
      api_key: sk-mesh-your-api-key # Key generated from the ollama-mesh dashboard
```

Replace:
- `<mesh-host>` with the hostname or IP address of the machine running ollama-mesh.
- `sk-mesh-your-api-key` with the API key created in the ollama-mesh dashboard.
- `llama3.2` with whatever model names are loaded on your local GPU nodes.

---

## Option B - Cloud Fallback Gateway Setup (ollama-mesh &rarr; LiteLLM)

If you already use LiteLLM to centralize your company's cloud LLM provider credentials (OpenAI, Anthropic, Groq, etc.), you can configure ollama-mesh to use LiteLLM as its single cloud fallback endpoint when local GPU nodes are saturated or offline.

This is managed dynamically through the admin dashboard interface and stored in the SQLite database.

```
Applications ────> ollama-mesh ──(local GPUs busy)──> LiteLLM ────> Cloud APIs
```

### Admin Dashboard Configuration
1. Open the ollama-mesh dashboard and navigate to the **Settings** page.
2. Scroll to the **Integrations & Cost** section.
3. Under **LiteLLM Integration**:
   - Toggle **Enable LiteLLM** to active.
   - Enter your **LiteLLM Endpoint URL** (e.g., `http://<litellm-host>:4000/v1`).
   - Enter the **API Key** (LiteLLM virtual or master key, sent as `Authorization: Bearer <key>` to the LiteLLM proxy).
4. Click **Save Settings** to persist the configuration to the SQLite store.

> [!IMPORTANT]
> Enabling **LiteLLM Integration** automatically disables and deactivates the native **Cloud Providers** config block on the settings page. You will see a banner indicating: `"Managed by LiteLLM while enabled - this list is inactive"`.
> All cloud fallback/overflow routing will be managed exclusively by the LiteLLM proxy.

---

## Docker Compose Setup

If running both gateways on the same host, use a shared Docker network and service names for hostnames:

```yaml
# docker-compose.yml
services:
  ollama-mesh:
    image: ghcr.io/anirudhx7/ollama-mesh:latest
    ports:
      - "11434:11434"
      - "8080:8080"
    volumes:
      - mesh-data:/root
    networks:
      - shared-network

  litellm:
    image: ghcr.io/berriai/litellm:main-latest
    ports:
      - "4000:4000"
    volumes:
      - ./litellm-config.yaml:/app/config.yaml
    command: [ "--config", "/app/config.yaml" ]
    depends_on:
      - ollama-mesh
    networks:
      - shared-network

networks:
  shared-network:
    driver: bridge

volumes:
  mesh-data:
```

Your `litellm-config.yaml` can then reference `http://ollama-mesh:11434/v1` as the `api_base`, and ollama-mesh's settings can use `http://litellm:4000/v1` as the `url`.

---

## Model Discovery

With Option A, LiteLLM queries the configured model base. Under the hood, LiteLLM calls the `/v1/models` endpoint of `ollama-mesh` on startup to populate its active models.

ollama-mesh aggregates loaded models across all healthy GPU nodes dynamically and responds to `/v1/models` in real-time. If a model is not currently warm on any node, it will still appear in the list if it is registered in `ollama-mesh`'s model configuration.

---

## Verification

### Upstream (LiteLLM &rarr; ollama-mesh)
1. Send a request to LiteLLM:
   ```bash
   curl http://localhost:4000/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer your-litellm-key" \
     -d '{
       "model": "llama3.2",
       "messages": [{"role": "user", "content": "Hello from LiteLLM!"}]
     }'
   ```
2. Verify the request appears in the ollama-mesh request log at `http://<mesh-host>:8080` with the correct model and node latency.

### Fallback (ollama-mesh &rarr; LiteLLM)
1. Drain or set all local nodes to offline via the ollama-mesh dashboard.
2. Send a request to ollama-mesh for a cloud model (e.g. `gpt-4o-mini`):
   ```bash
   curl http://localhost:11434/v1/chat/completions \
     -H "Authorization: Bearer sk-mesh-abc123" \
     -H "Content-Type: application/json" \
     -d '{
       "model": "gpt-4o-mini",
       "messages": [{"role": "user", "content": "Test fallback"}]
     }'
   ```
3. Check the LiteLLM logs: the request should be logged as incoming from the mesh and forwarded to the cloud provider.

---

## Troubleshooting

### 401 Unauthorized
- **Option A**: Verify the `api_key` in LiteLLM's `config.yaml` matches an active key listed in the ollama-mesh admin dashboard.
- **Option B**: Verify the API Key in the ollama-mesh Settings matches LiteLLM's master key or a valid virtual key.

### 403 Forbidden
The key you are using has a model allow-list configured in the ollama-mesh dashboard that excludes the requested model. Either adjust the model allow-list or use a key with access to all models.

### Connection Refused / Timeout
If running inside Docker, make sure both containers share a network. `localhost` inside a Docker container refers to the container itself; use the service name (e.g. `ollama-mesh` or `litellm`) or `host.docker.internal` for the host machine.
