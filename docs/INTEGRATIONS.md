# Integrations

ollama-mesh exposes an Ollama-compatible API on port 11434 and passes through Ollama's OpenAI-compatible `/v1` endpoints unchanged. This means any client that works with Ollama or the OpenAI SDK can point at the mesh with a one-line change.

Set `OPENAI_BASE_URL` (or the equivalent in your client) to `http://your-mesh-host:11434` and set the API key to your `sk-mesh-...` key.

---

## Python - openai SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:11434/v1",
    api_key="sk-mesh-abc123",   # your ollama-mesh key, not an OpenAI key
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
export OPENAI_API_KEY="sk-mesh-abc123"
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
  apiKey: "sk-mesh-abc123",
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
  -H "Authorization: Bearer sk-mesh-abc123" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:8b",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Streaming (`stream: true`):

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer sk-mesh-abc123" \
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
  -H "Authorization: Bearer sk-mesh-abc123"
```

---

## curl - Native Ollama endpoint

ollama-mesh also accepts the native Ollama `/api/chat` format:

```bash
curl http://localhost:11434/api/chat \
  -H "Authorization: Bearer sk-mesh-abc123" \
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
  -H "Authorization: Bearer sk-mesh-abc123" \
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
    api_key="sk-mesh-abc123",
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

## Notes

- Your `sk-mesh-...` key never leaves the mesh. The client `Authorization` header is stripped before forwarding to a local Ollama node, and replaced with the cloud provider's own configured `api_key` when a request overflows to cloud. Provider credentials live only in the database (encrypted).
- If your key has a `models:` allow-list configured, requests for any other model return `403 Forbidden`.
- Rate limit headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`) are present on every response and follow the same conventions as the OpenAI API.
- `GET /v1/models` returns the union of models loaded or downloaded across all healthy nodes.
