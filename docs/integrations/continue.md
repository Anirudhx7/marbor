# Continue (VS Code / JetBrains)

[Continue](https://continue.dev) is an open-source AI coding assistant. Point it at marbor and every completion request routes through warm-first selection across your GPU nodes, with cost-aware cloud overflow when nodes are busy.

---

## Prerequisites

- Continue extension installed (VS Code: `continue.continue`, JetBrains: Continue plugin)
- marbor running and reachable from your workstation
- An API key from the marbor admin dashboard (`http://<marbor-host>:8080`)

---

## Configuration

Open `~/.continue/config.json` (or use the Continue settings UI) and add an Ollama provider pointed at your marbor:

```json
{
  "models": [
    {
      "title": "Marbor - llama3.2",
      "provider": "ollama",
      "model": "llama3.2",
      "apiBase": "http://<marbor-host>:11434",
      "requestOptions": {
        "headers": {
          "Authorization": "Bearer sk-your-api-key"
        }
      }
    }
  ],
  "tabAutocompleteModel": {
    "title": "Marbor - qwen2.5-coder:1.5b",
    "provider": "ollama",
    "model": "qwen2.5-coder:1.5b",
    "apiBase": "http://<marbor-host>:11434",
    "requestOptions": {
      "headers": {
        "Authorization": "Bearer sk-your-api-key"
      }
    }
  }
}
```

Replace:
- `<marbor-host>` with the hostname or IP where marbor runs (e.g. `192.168.1.50` or `localhost`)
- `sk-your-api-key` with a key from the admin dashboard
- Model names with whatever models you have loaded on your nodes

---

## OpenAI-compatible alternative

If you prefer the OpenAI provider (useful when using cloud fallback models):

```json
{
  "models": [
    {
      "title": "Marbor",
      "provider": "openai",
      "model": "llama3.2",
      "apiBase": "http://<marbor-host>:11434/v1",
      "apiKey": "sk-your-api-key"
    }
  ]
}
```

The `/v1` path accepts OpenAI-format requests. Marbor routes them the same way as Ollama-native requests.

---

## Verification

Send a test completion from Continue's chat panel. Then check the admin dashboard request log at `http://<marbor-host>:8080` - you should see the request appear with the model name, routing node, and latency.

If auth fails, Continue will show a connection error. Double-check the `Authorization` header value matches a key listed in the admin dashboard.

---

## Multi-model setup

Add one entry per model you want to use. All entries can share the same `apiBase` and key:

```json
{
  "models": [
    {
      "title": "llama3.2 (8B)",
      "provider": "ollama",
      "model": "llama3.2",
      "apiBase": "http://192.168.1.50:11434",
      "requestOptions": { "headers": { "Authorization": "Bearer sk-your-api-key" } }
    },
    {
      "title": "deepseek-coder-v2",
      "provider": "ollama",
      "model": "deepseek-coder-v2",
      "apiBase": "http://192.168.1.50:11434",
      "requestOptions": { "headers": { "Authorization": "Bearer sk-your-api-key" } }
    },
    {
      "title": "mistral-nemo (cloud fallback)",
      "provider": "ollama",
      "model": "mistral-nemo",
      "apiBase": "http://192.168.1.50:11434",
      "requestOptions": { "headers": { "Authorization": "Bearer sk-your-api-key" } }
    }
  ]
}
```

The third entry will route to a cloud provider automatically if `mistral-nemo` is not loaded on any GPU node and you have cost-aware cloud overflow configured.

---

## Per-developer API keys

Create a separate API key for each developer in the admin dashboard. This lets you see per-user token usage and apply individual rate limits or model restrictions without sharing a single key.
