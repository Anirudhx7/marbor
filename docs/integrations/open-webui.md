# Open WebUI

Point Open WebUI at marbor instead of a single Ollama box and you get warm-first routing across all your GPU nodes, cost-aware cloud overflow when every node is busy, per-key auth and rate limits, and a usage dashboard - all with zero changes to Open WebUI itself. Open WebUI sends the same API calls it always has; the marbor handles which node actually runs the model.

> **Open WebUI versions**
>
> Menu names and connection screens change frequently between Open WebUI releases. This guide has been tested against Open WebUI 0.10.x. If your interface looks different, look for **Admin Panel > Settings > Connections**, or configure the provider using the Docker environment variables shown below - those values don't change between UI versions.

---

## Recommended: OpenAI-compatible connection

If authentication is enabled on the marbor (the default), use the **OpenAI-compatible** connection type. It has an explicit API key field and works consistently across current Open WebUI releases. The Ollama-native connection type does not consistently expose an API key field across Open WebUI versions - use it only if authentication is disabled on the marbor, or your Open WebUI version supports authenticated Ollama connections.

| Connection | Recommended when |
|------------|-------------------|
| OpenAI-compatible | Authentication enabled, API keys required, production deployments |
| Ollama-native | Simple LAN deployments, authentication disabled, maximum Ollama API compatibility |

Both protocols are served on the same port and both require your `sk-marbor-...` API key as a `Bearer` token when auth is enabled:

| Protocol | URL |
|----------|-----|
| OpenAI-compatible | `http://<marbor-host>:11434/v1` |
| Ollama-native | `http://<marbor-host>:11434` |

---

## Before configuring Open WebUI: verify connectivity

Do this first. If it fails, fix connectivity before touching the Open WebUI UI - a broken connection looks identical to a misconfigured one from inside Open WebUI's settings screen.

```bash
curl http://<marbor-host>:11434/v1/models \
  -H "Authorization: Bearer sk-marbor-..."
```

This is the same request Open WebUI makes to verify an OpenAI-compatible provider (`GET /v1/models`). If it returns JSON listing your models, Open WebUI will be able to discover them too. If it fails, see [Troubleshooting](#troubleshooting) below.

---

## Networking pitfalls

**localhost vs LAN IP** - `localhost` means the same machine only. If Open WebUI and marbor run on different machines, use the LAN IP or hostname of the machine running marbor.

```
Same machine:      http://localhost:11434
Different machine:  http://192.168.1.7:11434
```

**Docker networking** - if Open WebUI runs inside a Docker container, `localhost` refers to the container itself, not the Docker host. Use `host.docker.internal` (Docker Desktop) or the Docker host's LAN IP instead. If both marbor and Open WebUI run as containers in the same Compose file, use the marbor's service name (see the Compose example below).

**HTTP vs HTTPS** - unless you've configured a reverse proxy (Nginx, Traefik, Caddy) in front of it, marbor serves plain HTTP. Don't use `https://` unless you've explicitly set up TLS.

---

## Open WebUI Desktop vs Server

The Open WebUI **Desktop** app uses a built-in local runtime (llama.cpp) by default, and some Desktop releases don't expose the same provider configuration screen as the server edition. To point Open WebUI at marbor, use the Open WebUI **server** (Docker or standalone) rather than the Desktop app.

---

## Quick start: Docker Compose

The most common deployment - both services in one Compose file:

```yaml
services:
  marbor:
    image: ghcr.io/anirudhx7/marbor:latest
    ports:
      - "11434:11434"
      - "8080:8080"
    volumes:
      - marbor-data:/root   # persists marbor.db - add nodes/keys once via the dashboard

  open-webui:
    image: ghcr.io/open-webui/open-webui:main
    ports:
      - "3000:8080"
    environment:
      OPENAI_API_BASE_URL: "http://marbor:11434/v1"
      OPENAI_API_KEY: "sk-marbor-abc123"
    volumes:
      - open-webui:/app/backend/data
    depends_on:
      - marbor

volumes:
  open-webui:
  marbor-data:
```

## Quick start: docker run

```bash
docker run -d -p 3000:8080 \
  -e OPENAI_API_BASE_URL="http://<marbor-host>:11434/v1" \
  -e OPENAI_API_KEY="sk-marbor-abc123" \
  -v open-webui:/app/backend/data \
  --name open-webui \
  ghcr.io/open-webui/open-webui:main
```

Ollama-native, no-auth variant:

```bash
docker run -d -p 3000:8080 \
  -e OLLAMA_BASE_URL="http://<marbor-host>:11434" \
  -v open-webui:/app/backend/data \
  --name open-webui \
  ghcr.io/open-webui/open-webui:main
```

> `OLLAMA_BASE_URL` and `OPENAI_API_BASE_URL` are `ConfigVar` variables in Open WebUI - their values are persisted to the internal database on first launch. If you change them after the first run, set `ENABLE_PERSISTENT_CONFIG=False` to force Open WebUI to always read from the environment.

## Manual configuration (existing Open WebUI installation)

Menu locations shift between releases, so configure by value rather than by following exact click paths:

| Field | Value |
|-------|-------|
| **Base URL** | `http://<marbor-host>:11434/v1` |
| **API Key** | Your `sk-marbor-...` key from the marbor dashboard's **API Keys** page |

1. Open Open WebUI and go to **Admin Panel > Settings > Connections**.
2. Under the OpenAI-compatible connections section, click **Add Connection** (the `+` button).
3. Enter the **Base URL** and **API Key** from the table above and save.
4. Open WebUI calls `GET /v1/models` - the marbor returns the union of models across all healthy nodes. Models appear in the model dropdown immediately.

To use the Ollama-native connection type instead (only when marbor auth is disabled), go to **Connections > Ollama** and enter `http://<marbor-host>:11434` as the URL - no API key field is available in most current Open WebUI releases for this connection type.

---

## Verifying the connection

After saving the connection, confirm models appear in the Open WebUI model selector. The marbor admin dashboard at `http://<marbor-host>:8080` shows each request logged with the routing decision (which node, model, latency) so you can confirm Open WebUI traffic is flowing through.

---

## Troubleshooting

**Connection refused**

Test connectivity from the same machine running Open WebUI:

```bash
curl http://<marbor-host>:11434/v1/models
```

If this fails, in order, check:

- the marbor process is running
- port 11434 is listening
- you're using the correct IP address (see [Networking pitfalls](#networking-pitfalls) above)
- firewall rules aren't blocking the port
- Docker networking - `localhost` inside a container isn't the host

**Models not appearing in the dropdown**

- Confirm at least one node is reachable and has models loaded.
- Check the marbor admin dashboard to verify nodes show as healthy.
- The OpenAI connection type calls `/v1/models`; the Ollama connection type calls `/api/tags`. Both are served by the marbor.
- If you configured the connection via Docker env var and models still don't appear, see the `ConfigVar` note above - a persisted value from a previous run may be overriding your env var.

**401 Unauthorized**

Your API key is missing or wrong. Verify the key matches one of the keys shown on the marbor dashboard's **API Keys** page. The marbor requires the exact key string as a `Bearer` token - no substring matching.

**403 Forbidden on a specific model**

The key you are using has a model allow-list set (edit it from the **API Keys** page) that does not include the requested model. Either add the model to the key's allow-list or use a key with no allow-list ("All models" - allow all).

**Slow model list on startup**

If a marbor node is unreachable, Open WebUI may wait for a timeout before the model list loads. Tune the marbor's upstream timeout in **Settings > Advanced Routing > Upstream Timeout**, or in Open WebUI set `AIOHTTP_CLIENT_TIMEOUT_MODEL_LIST=3` (seconds) to fail fast on unreachable endpoints.
