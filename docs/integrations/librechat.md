# LibreChat

[LibreChat](https://librechat.ai) supports Ollama as a custom endpoint. Point it at marbor to get warm-first routing across multiple GPU nodes, cost-aware cloud overflow, and full request logging - with no changes to how LibreChat works.

---

## Prerequisites

- LibreChat running (Docker or bare-metal)
- marbor running and reachable from the LibreChat host
- An API key from the marbor admin dashboard (`http://<marbor-host>:8080`)

---

## librechat.yaml configuration

LibreChat reads endpoint config from `librechat.yaml`. Add an Ollama custom endpoint:

```yaml
endpoints:
  custom:
    - name: "Marbor"
      apiKey: "sk-your-api-key"
      baseURL: "http://<marbor-host>:11434/v1"
      models:
        default: ["llama3.2", "mistral-nemo", "deepseek-coder-v2"]
        fetch: true
      titleConvo: true
      titleModel: "llama3.2"
      summarize: false
      forcePrompt: false
      dropParams:
        - "stop"
      headers:
        Authorization: "Bearer sk-your-api-key"
```

Replace:
- `<marbor-host>` with the hostname or IP of the marbor instance
- `sk-your-api-key` with a key from the admin dashboard
- The model list with models available on your cluster

The `baseURL` uses `/v1` (OpenAI-compatible path). marbor translates these to Ollama-native requests internally.

---

## Docker Compose setup

If LibreChat and marbor are on the same Docker host, use the service name as the host:

```yaml
# In docker-compose.override.yml
services:
  api:
    environment:
      - ENDPOINTS=custom
    volumes:
      - ./librechat.yaml:/app/librechat.yaml
```

`librechat.yaml` with Docker networking:

```yaml
endpoints:
  custom:
    - name: "Marbor"
      apiKey: "sk-your-api-key"
      baseURL: "http://marbor:11434/v1"
      models:
        default: ["llama3.2"]
        fetch: true
      headers:
        Authorization: "Bearer sk-your-api-key"
```

Add marbor to the same Docker network:

```yaml
# In marbor docker-compose section
services:
  marbor:
    image: ghcr.io/anirudhx7/marbor:latest
    ports:
      - "11434:11434"
      - "8080:8080"
    networks:
      - librechat_default
```

---

## Model discovery

With `fetch: true`, LibreChat calls `GET /v1/models` on startup to populate the model list. marbor implements this endpoint - it returns all models currently loaded across healthy nodes.

If `fetch: true` returns no models (nodes not yet ready), fall back to a static `default` list.

---

## Verification

1. Start LibreChat and select "Marbor" from the endpoint dropdown.
2. Send a test message.
3. Check the marbor request log at `http://<marbor-host>:8080` - the request should appear with model, node, and latency.

If you see a 401, the `Authorization` header in `librechat.yaml` is missing or the key has been revoked. Regenerate from the admin dashboard.

---

## Per-user keys

For teams, create one API key per user (or one per LibreChat role) in the admin dashboard. This separates usage stats and lets you apply rate limits or model restrictions per key without affecting the shared LibreChat config.
