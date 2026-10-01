# Integrations

marbor exposes an Ollama-compatible API on port 11434 and passes through Ollama's OpenAI-compatible `/v1` endpoints unchanged. This means any client that works with Ollama or the OpenAI SDK can point at marbor with a one-line change.

Set `OPENAI_BASE_URL` (or the equivalent in your client) to `http://your-marbor-host:11434` and set the API key to your `sk-marbor-...` key.

---

## Python - openai SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:11434/v1",
    api_key="sk-marbor-abc123",   # your marbor key, not an OpenAI key
)

response = client.chat.completions.create(
    model="llama3.2:8b",
    messages=[{"role": "user", "content": "What is 2 + 2?"}],
    stream=False,
)
print(response.choices[0].message.content)
```

Streaming:

```python
with client.chat.completions.create(
    model="llama3.2:8b",
    messages=[{"role": "user", "content": "Write a haiku about distributed systems."}],
    stream=True,
) as stream:
    for chunk in stream:
        delta = chunk.choices[0].delta.content
        if delta:
            print(delta, end="", flush=True)
```

Environment variable approach (recommended for scripts and agents):

```bash
export OPENAI_BASE_URL="http://localhost:11434/v1"
export OPENAI_API_KEY="sk-marbor-abc123"
```

```python
# No base_url/api_key needed - SDK reads from environment
from openai import OpenAI
client = OpenAI()
```

---

## Node.js - openai SDK

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:11434/v1",
  apiKey: "sk-marbor-abc123",
});

const response = await client.chat.completions.create({
  model: "llama3.2:8b",
  messages: [{ role: "user", content: "Explain warm-first routing in one sentence." }],
});

console.log(response.choices[0].message.content);
```

Streaming:

```typescript
const stream = await client.chat.completions.create({
  model: "llama3.2:8b",
  messages: [{ role: "user", content: "List three uses of a load balancer." }],
  stream: true,
});

for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
```

---

## curl - OpenAI-compatible endpoint

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-marbor-abc123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:8b",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Streaming (`stream: true`):

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-marbor-abc123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:8b",
    "messages": [{"role": "user", "content": "Count to 5"}],
    "stream": true
  }'
```

List available models (aggregated from all nodes):

```bash
curl http://localhost:11434/v1/models \
  -H "Authorization: Bearer sk-marbor-abc123"
```

---

## curl - Native Ollama endpoint

marbor also accepts the native Ollama `/api/chat` format:

```bash
curl http://localhost:11434/api/chat \
  -H "Authorization: Bearer sk-marbor-abc123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:8b",
    "messages": [{"role": "user", "content": "Hello from the Ollama API"}],
    "stream": false
  }'
```

Generate endpoint:

```bash
curl http://localhost:11434/api/generate \
  -H "Authorization: Bearer sk-marbor-abc123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:8b",
    "prompt": "The capital of France is",
    "stream": false
  }'
```

---

## LangChain (Python)

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    base_url="http://localhost:11434/v1",
    api_key="sk-marbor-abc123",
    model="llama3.2:8b",
    temperature=0,
)

response = llm.invoke("What is warm-first routing?")
print(response.content)
```

With streaming:

```python
for chunk in llm.stream("Explain cloud overflow in plain English."):
    print(chunk.content, end="", flush=True)
```

---

## Keeping existing model names (OpenWebUI, Cursor, apps built for cloud APIs)

Clients configured for a cloud model name such as `gpt-4` can keep that name. Declare a model alias that maps it to a real model on your fleet, and marbor rewrites each request before routing:

```bash
marbor models alias set gpt-4 llama3.2:8b
marbor models alias list
```

The same aliases are managed through the Admin API (`GET/PUT/DELETE /admin/model-aliases`) and the Routing page. Changes apply immediately, with no restart.

- Point the client at marbor (`http://your-marbor-host:11434/v1` for OpenAI-style clients such as OpenWebUI's OpenAI connection or Cursor's custom base URL) and leave its model name unchanged.
- `GET /v1/models` lists the alias (with its target's status) while the target is on the fleet, so model pickers that read that endpoint show it.
- Responses carry an `X-Marbor-Model-Alias: gpt-4 -> llama3.2:8b` header, and the request log shows `gpt-4 -> llama3.2:8b`. The response body's `model` field is the real model.
- Aliases apply to both `/v1/*` and the Ollama-native `/api/*` endpoints. See [LIMITATIONS.md](LIMITATIONS.md#model-aliases) for the full list of rules.

---

## Notes

- Your `sk-marbor-...` key never leaves the marbor. The client `Authorization` header is stripped before forwarding to a local Ollama node, and replaced with the cloud provider's own configured `api_key` when a request overflows to cloud. Provider credentials live only in the database (encrypted).
- If your key has a `models:` allow-list configured, requests for any other model return `403 Forbidden`.
- Rate limit headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`) are present on every response and follow the same conventions as the OpenAI API.
- `GET /v1/models` returns the union of models loaded or downloaded across all healthy nodes.
