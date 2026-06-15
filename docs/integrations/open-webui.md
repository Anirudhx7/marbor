# Open WebUI

Point Open WebUI at ollama-mesh instead of a single Ollama box and you get warm-first routing across all your GPU nodes, automatic cloud overflow when every node is busy, per-key auth and rate limits, and a usage dashboard - all with zero changes to Open WebUI itself. Open WebUI sends the same Ollama API calls it always has; the mesh handles which node actually runs the model.

---

## Connection options

ollama-mesh exposes two protocols on the same port:

| Protocol | URL | When to use |
|----------|-----|-------------|
| Ollama-native | `http://<mesh-host>:11434` | Recommended. Full Ollama API passthrough including `/api/chat`, `/api/generate`, `/api/pull`. |
| OpenAI-compatible | `http://<mesh-host>:11434/v1` | Use when connecting through Open WebUI's OpenAI connection type. |

Both paths require your `sk-mesh-...` API key sent as a `Bearer` token.

---

## Option A - Ollama connection type (recommended)

This is the best fit because Open WebUI treats the mesh exactly like a single Ollama instance and can enumerate models automatically.

1. Open Open WebUI and go to **Admin Settings** (gear icon, top-right).
2. Navigate to **Connections > Ollama**.
3. In the URL field, enter:
   ```
   http://<mesh-host>:11434
   ```
   Replace `<mesh-host>` with the IP or hostname of the machine running ollama-mesh. If Open WebUI is running in Docker on the same host, use `host.docker.internal` instead of `localhost`.
4. Open WebUI will probe `/api/tags` and `/v1/models` to enumerate models. The mesh aggregates model lists from all healthy nodes, so every model available across your fleet appears in one dropdown.
5. **API key**: Open WebUI's Ollama connection type does not send an `Authorization` header by default. If `auth.enabled: true` in your mesh config, you have two options:
   - Set `auth.enabled: false` and rely on network-level access control (only recommended on a trusted LAN).
   - Use the OpenAI connection type (Option B below), which has an explicit API key field.

---

## Option B - OpenAI-compatible connection type

Use this when you want to supply the mesh API key through the UI, or when you are already using Open WebUI's OpenAI connection slot.

1. Go to **Admin Settings > Connections > OpenAI** (in Open WebUI v0.5+, labeled under **Connections**).
2. Click **Add Connection** (the `+` button).
3. Fill in the fields:

   | Field | Value |
   |-------|-------|
   | **URL** | `http://<mesh-host>:11434/v1` |
   | **API Key** | Your `sk-mesh-...` key from `config.yaml` |

4. Click **Save**. Open WebUI calls `GET /v1/models` - the mesh returns the union of models across all healthy nodes. Models appear in the model dropdown immediately.

> The exact label for the OpenAI connection section has shifted between Open WebUI versions. Look for **Connections** under Admin Settings; the OpenAI slot is the one that has a separate **URL** and **API Key** field pair.

---

## Docker env vars

If you run Open WebUI via Docker and want to pre-configure the connection without touching the UI:

**Ollama-native (no auth):**
```bash
docker run -d -p 3000:8080 \
  -e OLLAMA_BASE_URL="http://<mesh-host>:11434" \
  -v open-webui:/app/backend/data \
  --name open-webui \
  ghcr.io/open-webui/open-webui:main
```

**OpenAI-compatible (with API key):**
```bash
docker run -d -p 3000:8080 \
  -e OPENAI_API_BASE_URL="http://<mesh-host>:11434/v1" \
  -e OPENAI_API_KEY="sk-mesh-abc123" \
  -v open-webui:/app/backend/data \
  --name open-webui \
  ghcr.io/open-webui/open-webui:main
```

> `OLLAMA_BASE_URL` and `OPENAI_API_BASE_URL` are `ConfigVar` variables in Open WebUI - their values are persisted to the internal database on first launch. If you change them after the first run, set `ENABLE_PERSISTENT_CONFIG=False` to force Open WebUI to always read from the environment.

**Docker Compose example (both running as services):**
```yaml
services:
  ollama-mesh:
    image: ghcr.io/anirudhx7/ollama-mesh:latest
    ports:
      - "11434:11434"
      - "8080:8080"
    volumes:
      - ./config.yaml:/config.yaml
    environment:
      CONFIG_PATH: /config.yaml

  open-webui:
    image: ghcr.io/open-webui/open-webui:main
    ports:
      - "3000:8080"
    environment:
      OPENAI_API_BASE_URL: "http://ollama-mesh:11434/v1"
      OPENAI_API_KEY: "sk-mesh-abc123"
    volumes:
      - open-webui:/app/backend/data
    depends_on:
      - ollama-mesh

volumes:
  open-webui:
```

---

## Verifying the connection

After saving the connection, confirm models appear in the Open WebUI model selector. You can also check the mesh side:

```bash
# Should return JSON with all models loaded across your nodes
curl http://<mesh-host>:11434/v1/models \
  -H "Authorization: Bearer sk-mesh-abc123"
```

The mesh admin dashboard at `http://<mesh-host>:8080` shows each request logged with the routing decision (which node, model, latency) so you can confirm Open WebUI traffic is flowing through.

---

## Troubleshooting

**Models not appearing in the dropdown**

- Confirm at least one Ollama node is reachable and has models loaded (`ollama list` on the node).
- Check the mesh admin dashboard to verify nodes show as healthy.
- For the Ollama connection type, Open WebUI calls `/api/tags`. For the OpenAI connection type it calls `/v1/models`. Both are served by the mesh.
- If you added the connection via Docker env var and models still don't appear, see the `ConfigVar` note above - the persisted value may be overriding your env var.

**401 Unauthorized**

Your API key is missing or wrong. Verify the key matches one of the `auth.keys[].key` values in `config.yaml`. The mesh requires the exact key string as a `Bearer` token - no substring matching.

**403 Forbidden on a specific model**

The key you are using has a `models:` allow-list in `config.yaml` that does not include the requested model. Either add the model to the key's allow-list or use a key with `models: []` (empty = allow all).

**Connection refused / timeout**

- If Open WebUI is in Docker and the mesh is on the host, use `http://host.docker.internal:11434` not `http://localhost:11434`.
- Confirm the mesh is running: `curl http://<mesh-host>:11434/api/tags` from the Open WebUI host (no auth needed for this endpoint if `auth.enabled: false`, or add the header if enabled).

**Slow model list on startup**

If a mesh node is unreachable, Open WebUI may wait for a timeout before the model list loads. Tune the mesh's `routing.upstream_timeout_ms` in `config.yaml`, or in Open WebUI set `AIOHTTP_CLIENT_TIMEOUT_MODEL_LIST=3` (seconds) to fail fast on unreachable endpoints.
